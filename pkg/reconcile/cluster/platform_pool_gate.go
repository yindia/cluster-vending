package cluster

import (
	"context"

	"github.com/yindia/cluster-vending/pkg/component"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The install order of a cluster's components (docs/architecture.md#node-layout,
// "Install order"):
//
//   - phase 1, on the system managed node group: the EKS add-ons, then
//     Karpenter (sequence 0) and what else places itself with ${system.*};
//   - phase 2, on Karpenter's platform pool: every component placed with
//     ${platform.*}. Its placement has no value until the pool is Ready
//     (its NodePool and EC2NodeClass, which reconcileNodeGroups applies
//     once Karpenter is Ready), so the component waits
//     (PlatformComponentPlan.WaitsFor) and its pods never sit Pending on a
//     pool that does not exist. What requires it waits with it.

// platformPoolWait is why the components on the platform pool wait: "" when
// they do not (the pool is Ready, as this pass or the last recorded it, or
// the cluster has no pool of the platform's: imported, or a config without
// one).
func (s *Reconciler) platformPoolWait(ctx context.Context, row *model.Cluster, imported bool) string {
	if imported || !s.platformPoolOn() {
		return ""
	}
	pool := storedPlatformPool(storedStatus(row))
	if r := nodeGroupsOfPass(ctx); r != nil && r.platformPool != nil {
		pool = r.platformPool
	}
	if pool.GetPhase() == cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY {
		return ""
	}
	why := "the platform pool " + crossplaneaws.PoolPlatform + " to be Ready (after Karpenter)"
	if msg := pool.GetMessage(); msg != "" {
		why += ": " + msg
	}
	return why
}

// storedPlatformPool is the platform pool's status as recorded; nil for none.
func storedPlatformPool(st *cloudv1.ClusterStatus) *cloudv1.NodeGroupStatus {
	for _, g := range st.GetNodeGroups() {
		if g.GetPlatformManaged() && g.GetName() == crossplaneaws.PoolPlatform {
			return g
		}
	}
	return nil
}

// withPlatformPool resolves ${platform.*} in vars: the platform pool's
// placement (as VarsFor has it) once the pool is Ready, a wait (poolWait)
// before; on a cluster without the platform's pool, the system group's live
// placement (live), as before the pool. An imported cluster keeps its
// default pool (live is nil).
func (s *Reconciler) withPlatformPool(vars component.Vars, live *component.Placement, poolWait string) component.Vars {
	switch {
	case live == nil:
		return vars
	case !s.platformPoolOn():
		return component.WithPlatformPlacement(vars, *live)
	case poolWait != "":
		return component.WaitForPlatformPool(vars, poolWait)
	}
	return vars
}
