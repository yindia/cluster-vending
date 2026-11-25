package cluster

import (
	"context"
	"fmt"
	"slices"

	"github.com/yindia/cluster-vending/pkg/component"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A cluster's deletion runs in the reverse of its creation, in stages; a
// stage starts only when the one before it is fully gone
// (docs/architecture.md#deletion):
//
//  1. load balancers in the workload cluster
//  2. tenant components, a component after those that require it
//  3. platform components above sequence 0, highest sequence first
//  4. the Karpenter node groups: node pools (the users', and an old platform
//     pool of a cluster built before the system group), node claims, node classes
//  5. Karpenter: its chart, its IAM, its queue, rules and node role
//  6. the EKS add-ons with their pod identity associations and roles
//  7. the rest, ordered by their Usages: the default node group, the EKS
//     cluster, the cluster's IAM, the results bucket, ...
const deleteStages = 7

const (
	stageLoadBalancers = iota + 1
	stageTenant
	stagePlatform
	stageNodeGroups
	stageKarpenter
	stageAddons
	stageRest
)

var stageNames = map[int]string{
	stageLoadBalancers: "load balancers", stageTenant: "tenant components", stagePlatform: "platform components",
	stageNodeGroups: "node groups", stageKarpenter: "Karpenter", stageAddons: "EKS add-ons", stageRest: "the cluster",
}

type deleteStageKey struct{}

type deleteStageHolder struct{ message string }

// withDeleteStage returns a context a pass's deletion leaves the stage it
// waits in.
func withDeleteStage(ctx context.Context) context.Context {
	return context.WithValue(ctx, deleteStageKey{}, &deleteStageHolder{})
}

// setDeleteStage records what the deletion is at: the stage and what it
// waits for. It is the deleting cluster's message for the pass.
func setDeleteStage(ctx context.Context, stage int, waits string) {
	if h, ok := ctx.Value(deleteStageKey{}).(*deleteStageHolder); ok {
		h.message = fmt.Sprintf("deleting; stage %d of %d, %s: %s", stage, deleteStages, stageNames[stage], waits)
	}
}

// appendDeleteStage adds text to the stage message the pass has set; nothing
// when it set none or the text is empty.
func appendDeleteStage(ctx context.Context, text string) {
	if h, ok := ctx.Value(deleteStageKey{}).(*deleteStageHolder); ok && h.message != "" && text != "" {
		h.message += "; " + text
	}
}

// withDeleteStageStatus makes the stage the deleting cluster's message.
func withDeleteStageStatus(ctx context.Context, st *cloudv1.ClusterStatus) {
	if h, ok := ctx.Value(deleteStageKey{}).(*deleteStageHolder); ok && h.message != "" &&
		st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_DELETING {
		st.Message = h.message
	}
}

// definitionOrder are the components' sequences and requirements, as the
// catalog has them now. A component the catalog no longer has is unknown:
// it is removed with the platform components of the default sequence. The
// NodeProvisioner stays sequence 0 (its stage is fixed): every definition
// that plays the role, or the shipped one's name when the catalog has none
// or cannot be read (component.RoleName). Without the catalog nothing else
// is known and the Usages alone order the platform components.
type definitionOrder struct {
	sequence     map[string]int32
	requires     map[string][]string
	provisioners map[string]bool
}

func (s *Reconciler) definitionOrder(ctx context.Context) definitionOrder {
	out := definitionOrder{sequence: map[string]int32{}, requires: map[string][]string{},
		provisioners: map[string]bool{component.RoleName(nil, component.RoleNodeProvisioner, ""): true}}
	if s.deps.Platform == nil {
		return out
	}
	objs, err := s.deps.Platform.Definitions(ctx)
	if err != nil {
		s.logger().WarnContext(ctx, "deletion: the component catalog cannot be read; components are removed in the order of their Usages", "error", err)
		return out
	}
	defs := component.DefinitionsOf(objs)
	for _, d := range defs {
		out.sequence[d.Name], out.requires[d.Name] = d.Sequence, d.Requires
	}
	if provisioners := component.WithRole(defs, component.RoleNodeProvisioner, ""); len(provisioners) > 0 {
		out.provisioners = map[string]bool{}
		for _, d := range provisioners {
			out.provisioners[d.Name] = true
		}
	}
	return out
}

// sequenceOf is a component's sequence: 0 for the NodeProvisioner (its
// stage is fixed, whatever the catalog says), else the catalog's, else the
// default.
func (o definitionOrder) sequenceOf(name string) int32 {
	if o.provisioners[name] {
		return component.NodeProvisionerSequence // first: a catalog that moved it would put it among the rest
	}
	if seq, ok := o.sequence[name]; ok {
		return seq
	}
	return component.DefaultSequence
}

// requiredBy reports whether any of others requires name.
func (o definitionOrder) requiredBy(name string, others []string) bool {
	for _, other := range others {
		if other != name && slices.Contains(o.requires[other], name) {
			return true
		}
	}
	return false
}
