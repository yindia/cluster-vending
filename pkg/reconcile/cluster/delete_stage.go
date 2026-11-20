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

// withDeleteStageStatus makes the stage the deleting cluster's message.
func withDeleteStageStatus(ctx context.Context, st *cloudv1.ClusterStatus) {
	if h, ok := ctx.Value(deleteStageKey{}).(*deleteStageHolder); ok && h.message != "" &&
		st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_DELETING {
		st.Message = h.message
	}
}

// definitionOrder are the components' sequences and requirements, as the
// catalog has them now. A component the catalog no longer has is unknown:
// it is removed with the platform components of the default sequence
// (Karpenter, by name, stays sequence 0). Without the catalog nothing is
// known and the Usages alone order the platform components.
type definitionOrder struct {
	sequence map[string]int32
	requires map[string][]string
}

func (s *Reconciler) definitionOrder(ctx context.Context) definitionOrder {
	out := definitionOrder{sequence: map[string]int32{}, requires: map[string][]string{}}
	if s.deps.Platform == nil {
		return out
	}
	defs, err := s.deps.Platform.Definitions(ctx)
	if err != nil {
		s.logger().WarnContext(ctx, "deletion: the component catalog cannot be read; components are removed in the order of their Usages", "error", err)
		return out
	}
	for i := range defs {
		d, err := component.DefinitionOf(&defs[i])
		if err != nil {
			continue
		}
		out.sequence[d.Name], out.requires[d.Name] = d.Sequence, d.Requires
	}
	return out
}

// sequenceOf is a component's sequence: the catalog's, else 0 for Karpenter,
// else the default.
func (o definitionOrder) sequenceOf(name string) int32 {
	if seq, ok := o.sequence[name]; ok {
		return seq
	}
	if name == karpenterComponent {
		return 0
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
