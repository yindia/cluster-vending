package cluster

import (
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// measure observes the long transitions between two recorded statuses of
// a cluster (as statusEvents reports them): its first RUNNING after
// provisioning (an import's is not a creation), a finished Kubernetes
// upgrade, and each platform component that went from INSTALLING to READY.
func measure(m reconcile.Metrics, row *model.Cluster, before, after *cloudv1.ClusterStatus, at time.Time) {
	if before.GetState() == cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING &&
		after.GetState() == cloudv1.ClusterState_CLUSTER_STATE_RUNNING &&
		after.GetImport() == nil && !row.CreatedAt.IsZero() {
		m.ClusterProvisioned(at.Sub(row.CreatedAt))
	}
	if was := before.GetUpgrade(); was != nil && after.GetUpgrade() == nil &&
		after.GetState() != cloudv1.ClusterState_CLUSTER_STATE_DELETING && was.GetStartTime() != nil {
		m.UpgradeFinished(at.Sub(was.GetStartTime().AsTime()))
	}
	installing := map[string]*cloudv1.PlatformComponent{}
	for _, c := range before.GetPlatformComponents() {
		if c.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING && c.GetSince() != nil {
			installing[c.GetName()] = c
		}
	}
	for _, c := range after.GetPlatformComponents() {
		if prev, ok := installing[c.GetName()]; ok && c.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			m.ComponentReady(c.GetName(), at.Sub(prev.GetSince().AsTime()))
		}
	}
}
