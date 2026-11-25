package cluster

import (
	"reflect"
	"strings"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var (
	poolSelector = map[string]any{crossplaneaws.PoolLabelKey: crossplaneaws.PoolPlatform}
	poolTaints   = []any{map[string]any{"key": crossplaneaws.PoolLabelKey, "operator": "Equal", "value": crossplaneaws.PoolPlatform, "effect": "NoSchedule"}}
)

// recordPoolPhase records the platform pool's phase on the cluster's
// status, as a pass's node-group step does.
func recordPoolPhase(t *testing.T, w world, phase cloudv1.NodeGroupPhase) {
	t.Helper()
	row := w.cluster(t)
	st := storedStatus(row)
	st.NodeGroups = []*cloudv1.NodeGroupStatus{groupStatus(crossplaneaws.PoolPlatform, true, phase, "", 0)}
	row.Status = mustStatus(t, st)
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
}

// The install order with a platform pool (docs/architecture.md#node-layout,
// "Install order"): phase 1, Karpenter on the system group; phase 2, a
// component placed with ${platform.*} waits, saying so, until the platform
// pool is Ready, then runs on the pool.
func TestPlatformPoolComponentsWaitForThePool(t *testing.T) {
	w, src := platformWorld(t)
	placementDefs(src)
	w.helmConfig(t)
	withPlatformPool(w)

	w.install(t)
	if w.release(t, "karpenter") == nil {
		t.Fatal("karpenter was not written first")
	}
	if sel, tol := placementOfRelease(t, w.release(t, "karpenter")); !reflect.DeepEqual(sel, systemSelector) || !reflect.DeepEqual(tol, systemTaints) {
		t.Errorf("karpenter placement = %v %v, want the system group", sel, tol)
	}
	w.deployed(t, "karpenter")

	for _, phase := range []cloudv1.NodeGroupPhase{cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_UNSPECIFIED, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING} {
		if phase != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_UNSPECIFIED {
			recordPoolPhase(t, w, phase)
		}
		p := w.install(t)

		if w.release(t, "workload") != nil {
			t.Fatalf("pool %v: workload was written before the platform pool was Ready", phase)
		}
		if msg := componentOf(p, "workload").GetMessage(); !strings.Contains(msg, "platform pool") {
			t.Errorf("pool %v: workload message %q does not say it waits for the platform pool", phase, msg)
		}
	}

	recordPoolPhase(t, w, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY)
	w.install(t)

	sel, tol := placementOfRelease(t, w.release(t, "workload"))
	if !reflect.DeepEqual(sel, poolSelector) || !reflect.DeepEqual(tol, poolTaints) {
		t.Errorf("workload placement = %v %v, want the platform pool", sel, tol)
	}
}
