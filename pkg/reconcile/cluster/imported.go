package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// ownCarriedOver switches the carried-over objects of a taken-over cluster
// to full management (own). Every apply of such a cluster does it, so a
// takeover that failed half-way is finished by the next one; an object
// already owned is left alone. Clusters the platform created, and
// imported ones before their takeover, have nothing to own.
func (s *Reconciler) ownCarriedOver(ctx context.Context, row *model.Cluster) error {
	if !row.Imported || row.State == StateImported || row.State == StateBlocked {
		return nil
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return err
	}
	for _, r := range records {
		if !r.CarriedOver {
			continue // rendered: the apply takes it over
		}
		if err := s.own(ctx, r); err != nil {
			return fmt.Errorf("%s %s: %w", r.Kind, r.Name, err)
		}
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

// own switches one carried-over object to full management, without Delete
// for one marked crossplaneaws.RetainAnnotation.
func (s *Reconciler) own(ctx context.Context, r model.ManagedResource) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
	// From the API server: a read-modify-write needs the current object.
	if err := s.deps.Reader().Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	policies := []string{"*"}
	if u.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true" {
		policies = policies[:0]
		for _, action := range crossplaneaws.PolicyNoDelete {
			policies = append(policies, string(action))
		}
	}
	if have, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "managementPolicies"); slices.Equal(have, policies) {
		return nil
	}
	if err := unstructured.SetNestedStringSlice(u.Object, policies, "spec", "managementPolicies"); err != nil {
		return err
	}
	return s.deps.Kube.Update(ctx, u)
}
