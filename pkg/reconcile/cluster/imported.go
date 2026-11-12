package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Stored states of an imported cluster.
const (
	// StateImported: ImportCluster created Observe-only objects; nothing is
	// managed until TakeOverCluster.
	StateImported = "CLUSTER_STATE_IMPORTED"
	// StateBlocked: the import plan has BLOCKING lines; nothing was
	// imported.
	StateBlocked = "CLUSTER_STATE_FAILED"
)

// importSet is an import's objects as a set: nothing waits (they only
// observe what exists), and a node group is applied with its desired size
// at creation only, like every node group (crossplaneaws.ClusterSet).
func importSet(objects []client.Object) sequence.Set {
	set := sequence.Of(objects...)
	for i, item := range set {
		if _, ok := item.Object.(*awseks.NodeGroup); ok {
			set[i].Adjust = crossplaneaws.DesiredSizeAtCreateOnly
		}
	}
	return set
}

// applyImport records and applies an import's objects (sequence.Engine).
// Pod identity associations then get their external name (identities: an
// ID only the cloud knows, by object name) set once, outside the apply: it
// is not part of any later render.
func (s *Reconciler) applyImport(ctx context.Context, row *model.Cluster, a *model.CloudAccount, objects []client.Object, identities map[string]string) error {
	owner := reconcile.Owner{TenantID: row.TenantID, CloudAccountID: a.ID, Kind: model.OwnerCluster, ID: row.ID}
	if _, err := s.deps.Sequence().Apply(ctx, importSet(objects), s.deps.Inventory(owner)); err != nil {
		s.logger().ErrorContext(ctx, "apply imported resources", "cluster_id", row.ID, "error", err)
		return applyError(err)
	}
	for _, obj := range objects {
		id, ok := identities[obj.GetName()]
		if !ok {
			continue
		}
		live := &awseks.PodIdentityAssociation{}
		if err := s.deps.Reader().Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
			return fault.New(fault.Internal, "could not read the imported pod identity association: "+err.Error())
		}
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[crossplaneaws.ExternalNameAnnotation] = id
		if err := s.deps.Kube.Update(ctx, live); err != nil {
			return fault.New(fault.Internal, "could not name the imported pod identity association: "+err.Error())
		}
	}
	return nil
}

// StoredImport is the import plan kept on the row (imported clusters), or
// nil.
func StoredImport(row *model.Cluster) *cloudv1.ImportStatus {
	if len(row.Status) == 0 {
		return nil
	}
	st := &cloudv1.ClusterStatus{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(row.Status, st); err != nil {
		return nil
	}
	return st.GetImport()
}

// Adoption is what the cluster keeps of the resources it was imported
// from; zero for a cluster the platform created.
func Adoption(row *model.Cluster) crossplaneaws.Adoption {
	var a crossplaneaws.Adoption
	if len(row.Adoption) > 0 {
		_ = json.Unmarshal(row.Adoption, &a) // written by insertImport
	}
	return a
}

// withCarriedOver is set (what the cluster's spec renders) preceded by the
// carried-over objects of a taken-over cluster: the objects of its import
// that no spec field describes (crossplane.CarriedOverAnnotation), from
// the import's stored objects, owned (crossplaneaws.Own: full management,
// without Delete for a retained one) and otherwise exactly as they were
// imported. A takeover therefore applies them as it applies everything
// else: nothing is read and edited in place, and nothing is looked up in
// the cloud. They are in the cluster's inventory as kept, so they are
// never removed for not being rendered, and are deleted with the cluster.
// An object the spec renders under the same name wins.
//
// Clusters the platform created, and imported ones before their takeover,
// carry nothing over. A cluster imported before its import's objects were
// kept with it, and that has carried-over objects, has nothing to render
// them from: it must be imported again.
func (s *Reconciler) withCarriedOver(ctx context.Context, row *model.Cluster, set sequence.Set) (sequence.Set, error) {
	if !row.Imported || row.State == StateImported || row.State == StateBlocked {
		return set, nil
	}
	objects, _, ok, err := storedImportObjects(row, s.deps.Kube.Scheme())
	if err != nil {
		s.logger().ErrorContext(ctx, "read the import's stored objects", "cluster_id", row.ID, "error", err)
		return nil, fault.New(fault.Internal, "could not read the objects the cluster was imported with")
	}
	if !ok {
		return set, s.nothingCarriedOver(ctx, row)
	}
	rendered := make(map[string]bool, len(set))
	for _, item := range set {
		rendered[fmt.Sprintf("%T/%s", item.Object, item.Object.GetName())] = true
	}
	var carried []client.Object
	for _, obj := range objects {
		if obj.GetAnnotations()[crossplane.CarriedOverAnnotation] != "true" || rendered[fmt.Sprintf("%T/%s", obj, obj.GetName())] {
			continue
		}
		crossplaneaws.Own(obj)
		carried = append(carried, obj)
	}
	return append(importSet(carried), set...), nil
}

// nothingCarriedOver fails for a taken-over cluster that has carried-over
// objects and no stored import to render them from.
func (s *Reconciler) nothingCarriedOver(ctx context.Context, row *model.Cluster) error {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return storeError(err)
	}
	if slices.ContainsFunc(records, func(r model.ManagedResource) bool { return r.CarriedOver }) {
		return fault.Newf(fault.FailedPrecondition,
			"cluster %s was imported before the objects of its import were kept with it; delete it and import the cluster again", row.ID)
	}
	return nil
}

// CarriedOverConflict refuses to turn on a setting that renders an object
// the cluster already carries over from its import: the IAM OIDC provider
// (spec.aws.irsa) and the control plane's log group. Both would be
// rendered under the carried-over object's name, without its cloud
// identity.
func (s *Reconciler) CarriedOverConflict(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec) error {
	irsa, logGroup := spec.GetAws().GetIrsa(), spec.GetAws().GetControlPlaneLogGroup().GetEnabled()
	if !row.Imported || (!irsa && !logGroup) {
		return nil
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return storeError(err)
	}
	for _, r := range records {
		switch {
		case !r.CarriedOver:
		case irsa && r.Kind == "OpenIDConnectProvider":
			return fault.New(fault.FailedPrecondition,
				"spec.aws.irsa: the cluster already has the IAM OIDC provider it was imported with ("+r.Name+"); it stays as it is")
		case logGroup && r.Kind == "Group":
			return fault.New(fault.FailedPrecondition,
				"spec.aws.control_plane_log_group: the cluster already has the log group it was imported with ("+r.Name+"); it stays as it is")
		}
	}
	return nil
}
