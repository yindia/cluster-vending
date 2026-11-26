package cluster

import (
	"context"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// setCloudSweep leaves the cloud sweep's state of this pass for the status
// it records (withDeleteClocksStatus).
func setCloudSweep(ctx context.Context, t *cloudv1.ClusterTeardown) {
	if h, ok := ctx.Value(teardownKey{}).(*teardownHolder); ok {
		h.sweep, h.sweepSet = t, true
	}
}

// setNodeTeardown leaves the node groups' stage's state of this pass (nil:
// the stage's clock is cleared) for the status it records.
func setNodeTeardown(ctx context.Context, t *cloudv1.ClusterTeardown) {
	if h, ok := ctx.Value(teardownKey{}).(*teardownHolder); ok {
		h.nodes, h.nodesSet = t, true
	}
}

// withDeleteClocksStatus puts the node groups' stage and the cloud sweep
// into a status being built: what this pass computed, else what was
// recorded, so their clocks survive passes and restarts.
func withDeleteClocksStatus(ctx context.Context, st, stored *cloudv1.ClusterStatus) {
	st.NodeTeardown, st.CloudSweep = stored.GetNodeTeardown(), stored.GetCloudSweep()
	h, ok := ctx.Value(teardownKey{}).(*teardownHolder)
	if !ok {
		return
	}
	if h.nodesSet {
		st.NodeTeardown = h.nodes
	}
	if h.sweepSet {
		st.CloudSweep = h.sweep
	}
}
