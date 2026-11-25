package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
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
	set, err := reconcile.WithRecordedNames(ctx, importSet(objects), s.deps.Kube.Scheme())
	if err != nil {
		return fault.New(fault.Internal, err.Error())
	}
	if _, err := s.deps.Sequence().Apply(ctx, set, s.deps.Inventory(owner)); err != nil {
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
// the import's stored objects, owned (crossplaneaws.OwnCarriedOver: full
// management, without Delete for a retained one and for policy attachments) and otherwise exactly as they were
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
		crossplaneaws.OwnCarriedOver(obj)
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
// (spec.aws.irsa) and the control plane's log group, both rendered under
// the carried-over object's name, without its cloud identity; and the
// OIDC identity provider (spec.access.oidc), of which the platform manages
// one per cluster (EKS itself takes one before Kubernetes 1.32, and wants
// issuers unique after).
func (s *Reconciler) CarriedOverConflict(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec) error {
	irsa, logGroup := spec.GetAws().GetIrsa(), spec.GetAws().GetControlPlaneLogGroup().GetEnabled()
	oidc := spec.GetAccess().GetOidc() != nil
	if !row.Imported || (!irsa && !logGroup && !oidc) {
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
		case oidc && r.Kind == "IdentityProviderConfig":
			return fault.New(fault.FailedPrecondition,
				"spec.access.oidc: the cluster already has the OIDC identity provider it was imported with ("+r.Name+"), and the platform manages one per cluster; it stays as it is")
		}
	}
	return nil
}

// ImportDrift lists what the imported cluster's objects report now that
// differs from the plan the import stored, in the fields full management
// would write: the Kubernetes version, the endpoint access and public
// CIDRs, the node groups' scaling and instance types, the add-ons'
// versions and the access entries' groups. The takeover applies the stored
// plan, so a difference means it would push old values (a version EKS
// refuses, reverted CIDRs, an add-on rolled back). A field the object does
// not report yet is not a difference. Empty when the row has no stored
// objects (a cluster the platform created, or an older import).
//
// A planned object that cannot be owned as planned refuses the takeover
// with FailedPrecondition instead. One gone from the management cluster,
// or whose provider reports its cloud resource missing or failing
// (deleted in AWS since: owned, it would be created again), says to import
// again. One the provider has not reported on yet says to wait and retry:
// nothing changed, the import is still observing it.
func (s *Reconciler) ImportDrift(ctx context.Context, row *model.Cluster) ([]string, error) {
	planned, _, ok, err := storedImportObjects(row, s.deps.Kube.Scheme())
	if err != nil || !ok {
		return nil, err
	}
	var diffs, gone, pending []string
	for _, obj := range planned {
		live, st, err := s.liveOf(ctx, obj)
		if err != nil {
			return nil, err
		}
		switch st.kind {
		case liveGone, liveFailing:
			gone = append(gone, st.why)
		case livePending:
			pending = append(pending, st.why)
		default:
			if live != nil {
				diffs = append(diffs, driftOf(obj, live)...)
			}
		}
	}
	if len(gone) > 0 {
		return nil, fault.New(fault.FailedPrecondition, fmt.Sprintf(
			"resources of the import are gone since it was imported, and taking over would create them again: %s; delete the import and import it again to review the current plan",
			strings.Join(gone, "; ")))
	}
	if len(pending) > 0 {
		return nil, fault.New(fault.FailedPrecondition, fmt.Sprintf(
			"the import has not finished observing its resources: %s; wait for the import to finish observing its resources, then retry the takeover (cluster get --live)",
			strings.Join(pending, "; ")))
	}
	return diffs, nil
}

// liveKind says whether a planned object can be owned as planned.
type liveKind int

const (
	// liveObserved: its provider observes the cloud resource.
	liveObserved liveKind = iota
	// liveGone: the object is gone from the management cluster.
	liveGone
	// liveFailing: its provider reports the cloud resource missing or
	// failing (Synced=False).
	liveFailing
	// livePending: its provider has not reported on it yet.
	livePending
)

// liveState is a planned object's liveKind, and why for the refusal (it
// names the object).
type liveState struct {
	kind liveKind
	why  string
}

// liveOf reads the object as the management cluster has it now. st says
// whether it can be owned as planned; live is set for the kinds
// ImportDrift compares, once observed.
func (s *Reconciler) liveOf(ctx context.Context, planned client.Object) (live client.Object, st liveState, err error) {
	ref, err := managed.RefOf(planned, s.deps.Kube.Scheme())
	if err != nil {
		return nil, st, fault.New(fault.Internal, err.Error())
	}
	u, err := s.deps.Objects().Read(ctx, ref)
	if err != nil {
		return nil, st, fault.New(fault.Unavailable, fmt.Sprintf("cannot read %s %s: %v", ref.Kind, ref.Name, err))
	}
	if u == nil {
		return nil, liveState{liveGone, fmt.Sprintf("%s %s: gone from the management cluster", ref.Kind, ref.Name)}, nil
	}
	if st = observed(ref, u); st.kind != liveObserved {
		return nil, st, nil
	}
	switch planned.(type) {
	case *awseks.Cluster:
		live = &awseks.Cluster{}
	case *awseks.NodeGroup:
		live = &awseks.NodeGroup{}
	case *awseks.Addon:
		live = &awseks.Addon{}
	case *awseks.AccessEntry:
		live = &awseks.AccessEntry{}
	default:
		return nil, st, nil
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, live); err != nil {
		return nil, st, fault.New(fault.Internal, fmt.Sprintf("%s %s: %v", ref.Kind, ref.Name, err))
	}
	return live, st, nil
}

// observed says whether u's provider observes its cloud resource: Ready
// (or UPDATING) and Synced neither False nor Unknown. An Observe-only
// object whose resource is deleted keeps Ready and turns Synced=False
// ("external resource does not exist"): failing, with the provider's
// message. No conditions yet, Synced Unknown, a reference not resolved yet
// (WAITING) or not Ready yet: not reported on yet.
func observed(ref managed.Ref, u *unstructured.Unstructured) liveState {
	v := managed.View(ref, u)
	state := v.GetState()
	name := ref.Kind + " " + ref.Name
	if v.GetSynced() == cloudv1.ConditionStatus_CONDITION_STATUS_FALSE &&
		state != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING {
		why := name + ": Crossplane reports its cloud resource missing or failing"
		if m := conditionMessage(u, "Synced"); m != "" {
			why += " (" + m + ")"
		}
		return liveState{liveFailing, why}
	}
	ready := state == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY ||
		state == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING
	if ready && v.GetSynced() != cloudv1.ConditionStatus_CONDITION_STATUS_FALSE &&
		v.GetSynced() != cloudv1.ConditionStatus_CONDITION_STATUS_UNKNOWN {
		return liveState{kind: liveObserved}
	}
	why := name + ": Crossplane has not reported on its cloud resource yet"
	if m := v.GetMessage(); m != "" {
		why += " (" + m + ")"
	}
	return liveState{livePending, why}
}

// conditionMessage is the message of u's condition of type t.
func conditionMessage(u *unstructured.Unstructured, t string) string {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, _ := c.(map[string]any); m["type"] == t {
			msg, _ := m["message"].(string)
			return msg
		}
	}
	return ""
}

// driftOf compares a planned object with the same object as observed.
func driftOf(planned, live client.Object) []string {
	var d driftList
	switch p := planned.(type) {
	case *awseks.Cluster:
		l := live.(*awseks.Cluster)
		at := l.Status.AtProvider
		d.name = "Cluster " + p.Name
		d.strings("version", p.Spec.ForProvider.Version, at.Version)
		if v, o := p.Spec.ForProvider.VPCConfig, at.VPCConfig; v != nil && o != nil {
			d.bools("private endpoint access", v.EndpointPrivateAccess, o.EndpointPrivateAccess)
			d.bools("public endpoint access", v.EndpointPublicAccess, o.EndpointPublicAccess)
			d.sets("public access CIDRs", v.PublicAccessCidrs, o.PublicAccessCidrs)
		}
	case *awseks.NodeGroup:
		l := live.(*awseks.NodeGroup)
		at := l.Status.AtProvider
		d.name = "NodeGroup " + p.Name
		if v, o := p.Spec.ForProvider.ScalingConfig, at.ScalingConfig; v != nil && o != nil {
			d.floats("min size", v.MinSize, o.MinSize)
			d.floats("max size", v.MaxSize, o.MaxSize)
			// The desired size is written at creation only (DesiredSizeAtCreateOnly).
		}
		d.sets("instance types", p.Spec.ForProvider.InstanceTypes, at.InstanceTypes)
	case *awseks.Addon:
		l := live.(*awseks.Addon)
		d.name = "Addon " + p.Name
		d.strings("version", p.Spec.ForProvider.AddonVersion, l.Status.AtProvider.AddonVersion)
	case *awseks.AccessEntry:
		l := live.(*awseks.AccessEntry)
		d.name = "AccessEntry " + p.Name
		d.sets("kubernetes groups", p.Spec.ForProvider.KubernetesGroups, l.Status.AtProvider.KubernetesGroups)
	}
	return d.out
}

// driftList collects the differences of one object. A value that is unset
// on either side is not compared.
type driftList struct {
	name string
	out  []string
}

func (d *driftList) add(field, planned, observed string) {
	d.out = append(d.out, fmt.Sprintf("%s: %s is %s in the import plan, %s now", d.name, field, planned, observed))
}

func (d *driftList) strings(field string, planned, observed *string) {
	if planned != nil && observed != nil && *planned != *observed {
		d.add(field, *planned, *observed)
	}
}

func (d *driftList) bools(field string, planned, observed *bool) {
	if planned != nil && observed != nil && *planned != *observed {
		d.add(field, fmt.Sprint(*planned), fmt.Sprint(*observed))
	}
}

func (d *driftList) floats(field string, planned, observed *float64) {
	if planned != nil && observed != nil && *planned != *observed {
		d.add(field, fmt.Sprint(*planned), fmt.Sprint(*observed))
	}
}

func (d *driftList) sets(field string, planned, observed []*string) {
	if len(observed) == 0 {
		return
	}
	p, o := sortedValues(planned), sortedValues(observed)
	if !slices.Equal(p, o) {
		d.add(field, "["+strings.Join(p, ", ")+"]", "["+strings.Join(o, ", ")+"]")
	}
}

func sortedValues(in []*string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != nil {
			out = append(out, *v)
		}
	}
	slices.Sort(out)
	return out
}
