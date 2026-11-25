package cluster

import (
	"reflect"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// poolPass is a pass of the platform components with the platform pool
// recorded at phase, as the node-group step of the same pass records it.
func poolPass(t *testing.T, w world, row *model.Cluster, phase cloudv1.NodeGroupPhase) *model.Cluster {
	t.Helper()
	st := storedStatus(row)
	st.NodeGroups = []*cloudv1.NodeGroupStatus{groupStatus(crossplaneaws.PoolPlatform, true, phase, "", 0)}
	row.Status = mustStatus(t, st)
	row = w.pass(t, row)
	st = storedStatus(row)
	st.NodeGroups = []*cloudv1.NodeGroupStatus{groupStatus(crossplaneaws.PoolPlatform, true, phase, "", 0)}
	row.Status = mustStatus(t, st)
	return row
}

// A component placed with ${platform.*} that was installed and Ready on the
// system group before the platform pool existed moves onto the pool once
// the pool is Ready, also when the pool's state flapped meanwhile.
func TestInstalledPlatformComponentMovesOntoThePool(t *testing.T) {
	for _, tt := range []struct {
		name   string
		phases []cloudv1.NodeGroupPhase
		// redefined: the definition placed itself with ${system.*} and a
		// new catalog moves it to ${platform.*}, the pool Ready already.
		redefined bool
	}{
		{name: "definition moves to the pool", redefined: true},
		{name: "pool becomes Ready", phases: []cloudv1.NodeGroupPhase{cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY}},
		{name: "pool flaps", phases: []cloudv1.NodeGroupPhase{cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY,
			cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, src := platformWorld(t)
			cp := &catalogPlatform{fakePlatform: src, admitted: true}
			w.svc.deps.Platform = cp
			placementDefs(src)
			if tt.redefined {
				withPlatformPool(w)
				cp.setValues(t, "workload", `{"nodeSelector":"${system.nodeSelector}","tolerations":"${system.tolerations}"}`)
			}
			w.helmConfig(t)
			row := w.cluster(t)
			spec, err := SpecOf(row)
			if err != nil {
				t.Fatal(err)
			}
			spec.ReleaseChannel = cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE
			if row.Spec, err = Encode(spec); err != nil {
				t.Fatal(err)
			}
			// Installed before the pool: both on the system group.
			ready := cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY
			row = poolPass(t, w, row, ready)
			w.deployed(t, "karpenter")
			row = poolPass(t, w, row, ready)
			w.deployed(t, "workload")
			row = poolPass(t, w, row, ready)
			if sel, _ := placementOfRelease(t, w.release(t, "workload")); !reflect.DeepEqual(sel, systemSelector) {
				t.Fatalf("installed placement = %v", sel)
			}
			if got := phases(storedPlatform(row)); got != "karpenter=READY,workload=READY" {
				t.Fatalf("installed: %s", got)
			}

			if tt.redefined {
				cp.setValues(t, "workload", `{"nodeSelector":"${platform.nodeSelector}","tolerations":"${platform.tolerations}"}`)
			}
			withPlatformPool(w)
			for _, phase := range tt.phases {
				row = poolPass(t, w, row, phase)
			}
			for range 4 {
				row = poolPass(t, w, row, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY)
				if u := w.release(t, "workload"); u != nil {
					if sel, _ := placementOfRelease(t, u); reflect.DeepEqual(sel, poolSelector) {
						w.deployed(t, "workload")
					}
				}
			}
			sel, tol := placementOfRelease(t, w.release(t, "workload"))
			if !reflect.DeepEqual(sel, poolSelector) || !reflect.DeepEqual(tol, poolTaints) {
				t.Fatalf("workload placement = %v %v, want the platform pool", sel, tol)
			}
			if sel, _ := placementOfRelease(t, w.release(t, "karpenter")); !reflect.DeepEqual(sel, systemSelector) {
				t.Errorf("karpenter placement = %v", sel)
			}
		})
	}
}
