package network

import (
	"context"
	"fmt"
	"sort"
	"strings"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Lifecycle states stored on the network row. FAILED is observed from the
// managed resources, not stored (but for a blocked import).
const (
	StateProvisioning = "NETWORK_STATE_PROVISIONING"
	// StateUpdating: a change (labels, flow logs) is being applied.
	StateUpdating = "NETWORK_STATE_UPDATING"
	// StateReady: the loop saw every resource ready (settle) and leaves the
	// network alone until it changes.
	StateReady    = "NETWORK_STATE_READY"
	StateDeleting = "NETWORK_STATE_DELETING"
	// StateImported: ImportNetwork created Observe-only resources; nothing
	// is managed until TakeOverNetwork.
	StateImported = "NETWORK_STATE_IMPORTED"
	// StateBlocked: the import plan has BLOCKING lines; nothing was
	// imported.
	StateBlocked = "NETWORK_STATE_FAILED"
)

// Condition types of NetworkStatus.
const (
	ConditionReady  = "Ready"
	ConditionSynced = "Synced"
)

// observed is what the network's managed resources report: how many
// exist (for finalizing deletes) and their cloud IDs.
type observed struct {
	total     int
	resources *cloudv1.NetworkResources
	// objects are the live objects, in creation order.
	objects []client.Object
}

// observe reads the network's managed resources (by label, in the tenant
// namespace) for their readiness and cloud IDs.
func (s *Reconciler) observe(ctx context.Context, tenantID, networkID string) (observed, error) {
	out := observed{resources: &cloudv1.NetworkResources{}}
	opts := []client.ListOption{
		client.InNamespace(kube.TenantNamespace(tenantID)),
		client.MatchingLabels{crossplane.LabelNetwork: networkID},
	}
	for _, list := range crossplaneaws.NetworkLists() {
		if err := s.deps.List(ctx, list, opts...); err != nil {
			if meta.IsNoMatchError(err) {
				continue // provider not installed: nothing of this kind exists
			}
			return observed{}, fmt.Errorf("list %T: %w", list, err)
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return observed{}, err
		}
		for _, item := range items {
			out.add(item)
			if obj, ok := item.(client.Object); ok {
				out.objects = append(out.objects, obj)
			}
		}
	}
	r := out.resources
	for _, ids := range [][]string{r.PublicSubnetIds, r.PrivateSubnetIds, r.NatGatewayIds, r.EgressIps} {
		sort.Strings(ids)
	}
	return out, nil
}

func (o *observed) add(item any) {
	o.total++
	r := o.resources
	switch m := item.(type) {
	case *awsec2.VPC:
		r.CloudNetworkId = deref(m.Status.AtProvider.ID)
	case *awsec2.Subnet:
		id := deref(m.Status.AtProvider.ID)
		if id == "" {
			return
		}
		if m.Labels[crossplane.LabelSubnetRole] == string(crossplaneaws.SubnetPublic) {
			r.PublicSubnetIds = append(r.PublicSubnetIds, id)
		} else {
			r.PrivateSubnetIds = append(r.PrivateSubnetIds, id)
		}
	case *awsec2.NATGateway:
		if id := deref(m.Status.AtProvider.ID); id != "" {
			r.NatGatewayIds = append(r.NatGatewayIds, id)
		}
	case *awsec2.EIP:
		if ip := deref(m.Status.AtProvider.PublicIP); ip != "" {
			r.EgressIps = append(r.EgressIps, ip)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// status combines the stored lifecycle state with the state of each
// managed resource:
//
//	DELETING      stored: DeleteNetwork ran, resources are going away
//	FAILED        a resource FAILED (or went MISSING)
//	READY         every resource READY
//	PROVISIONING  otherwise (CREATING / WAITING resources)
func status(row *model.Network, views []*cloudv1.ManagedResource, resources *cloudv1.NetworkResources, clusterIDs []string) *cloudv1.NetworkStatus {
	st := &cloudv1.NetworkStatus{Resources: resources, ClusterIds: clusterIDs,
		ManagedResources: reconcile.ExplainMissing(views, row.State == StateDeleting),
		Import:           StoredImport(row)}
	ready, waiting := 0, 0
	var failed *cloudv1.ManagedResource
	for _, v := range views {
		switch v.GetState() {
		case cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY:
			ready++
		case cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING:
			waiting++
		case cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING:
			if failed == nil {
				failed = v
			}
		default: // CREATING, DELETING: in progress
		}
	}
	progress := fmt.Sprintf("%d of %d resources ready", ready, len(views))
	if waiting > 0 {
		progress += fmt.Sprintf(", %d waiting on others", waiting)
	}
	readyCond := &cloudv1.Condition{Type: ConditionReady, Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: "ResourcesNotReady", Message: progress}
	switch {
	case row.State == StateDeleting:
		st.State = cloudv1.NetworkState_NETWORK_STATE_DELETING
		st.Message = fmt.Sprintf("deleting; %d resources left", resourcesLeft(views))
		if kinds := crossplaneaws.NetworkDeleteOrder.Current(views); len(kinds) > 0 {
			st.Message = fmt.Sprintf("deleting %s; %d resources left", strings.Join(kinds, ", "), resourcesLeft(views))
		}
		if hint := dependencyHint(views); hint != "" {
			st.Message += "; " + hint
		}
	case row.State == StateBlocked:
		st.State = cloudv1.NetworkState_NETWORK_STATE_FAILED
		st.Message = "import blocked: " + firstBlocking(st.GetImport())
	case failed != nil:
		st.State = cloudv1.NetworkState_NETWORK_STATE_FAILED
		st.Message = fmt.Sprintf("%s %s: %s", failed.GetKind(), failed.GetName(), failed.GetMessage())
		st.Conditions = append(st.Conditions, &cloudv1.Condition{Type: ConditionSynced,
			Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "ReconcileError", Message: st.Message})
	case Discovering(row):
		st.State, st.Message = cloudv1.NetworkState_NETWORK_STATE_IMPORTED, discoveringMessage
	case row.State == StateImported:
		st.State = cloudv1.NetworkState_NETWORK_STATE_IMPORTED
		st.Message = "imported read-only (" + progress + "); review status.import, then TakeOverNetwork"
	case len(views) > 0 && ready == len(views) && caughtUp(row, views):
		st.State = cloudv1.NetworkState_NETWORK_STATE_READY
		readyCond.Status, readyCond.Reason = cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, "AllResourcesReady"
		if imp := st.GetImport(); imp != nil && imp.GetPhase() == cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING {
			imp.Phase = cloudv1.ImportPhase_IMPORT_PHASE_CONVERGED
		}
	case row.State == StateUpdating || row.State == StateReady: // was ready: reconciling a change
		st.State = cloudv1.NetworkState_NETWORK_STATE_UPDATING
		st.Message = progress
	default:
		st.State = cloudv1.NetworkState_NETWORK_STATE_PROVISIONING
		st.Message = progress
	}
	st.Freeze = reconcile.FreezeProto(row.Frozen)
	if paused, note := reconcile.RowFreeze(row.Frozen, views); paused != nil {
		// A pause is the operator's: reported, never a failure. A paused
		// network says who paused it whatever its state.
		st.Conditions = append(st.Conditions, paused)
		if row.Frozen.Holds() || st.GetState() != cloudv1.NetworkState_NETWORK_STATE_READY {
			st.Message = strings.TrimPrefix(st.Message+note, "; ")
		}
	}
	withEditIgnored(st, row)
	st.Conditions = append([]*cloudv1.Condition{readyCond}, st.Conditions...)
	return st
}

// dependencyViolation is the error AWS answers a delete of a VPC, subnet,
// internet gateway or security group with while something still uses it.
const dependencyViolation = "DependencyViolation"

// dependencyHintMax bounds the AWS message quoted in the hint.
const dependencyHintMax = 240

// dependencyHint says, when AWS refuses to delete one of the network's
// resources because something still uses it, what AWS reports and where
// to look: most often a load balancer (with its network interfaces and
// security groups) that a cluster's controller made and that was not
// deleted with the cluster. Nothing the platform deletes removes it.
func dependencyHint(views []*cloudv1.ManagedResource) string {
	vpcID := "the VPC"
	for _, v := range views {
		if v.GetKind() == "VPC" && v.GetExternalName() != "" {
			vpcID = v.GetExternalName()
		}
	}
	for _, v := range views {
		msg := v.GetMessage()
		if !strings.Contains(msg, dependencyViolation) {
			continue
		}
		if len(msg) > dependencyHintMax {
			msg = msg[:dependencyHintMax] + "..."
		}
		return fmt.Sprintf("the VPC still has dependencies AWS reports (%s %s: %s); look for load balancers, "+
			"network interfaces and security groups in %s that the platform did not create (left by a deleted cluster's "+
			"controllers) and delete them", v.GetKind(), v.GetName(), msg, vpcID)
	}
	return ""
}

// caughtUp reports whether the network's generation is applied and every
// resource's provider has seen the object as it is now. A resource that
// was Ready before a change stays Ready while the provider has not looked
// at the change yet: that is not READY.
func caughtUp(row *model.Network, views []*cloudv1.ManagedResource) bool {
	if row.AppliedGeneration < row.Generation {
		return false
	}
	for _, v := range views {
		if v.GetSynced() == cloudv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED { // no provider status: not a managed resource
			continue
		}
		if reconcile.Behind(v) {
			return false
		}
	}
	return true
}

func resourcesLeft(views []*cloudv1.ManagedResource) int {
	n := 0
	for _, v := range views {
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			n++
		}
	}
	return n
}

// StoredImport is the import plan kept on the row (imported networks), or
// nil.
func StoredImport(row *model.Network) *cloudv1.ImportStatus {
	if len(row.Status) == 0 {
		return nil
	}
	st := &cloudv1.NetworkStatus{}
	// Lenient: whether a network is imported decides if it may ever be
	// rendered from its spec. A field the contract dropped must not turn
	// an imported network into one that is.
	if err := Lenient.Unmarshal(row.Status, st); err != nil {
		return nil
	}
	return st.GetImport()
}

func firstBlocking(imp *cloudv1.ImportStatus) string {
	for _, l := range imp.GetResources() {
		if l.GetDisposition() == cloudv1.Disposition_DISPOSITION_BLOCKING {
			return l.GetDetail()
		}
	}
	return "see status.import"
}
