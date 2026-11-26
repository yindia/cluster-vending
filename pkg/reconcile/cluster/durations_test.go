package cluster

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

type recordedMetrics struct {
	provisioned, upgraded []time.Duration
	ready                 map[string]time.Duration
	results               []string
}

func (r *recordedMetrics) ClusterProvisioned(d time.Duration) {
	r.provisioned = append(r.provisioned, d)
}
func (r *recordedMetrics) UpgradeFinished(d time.Duration) { r.upgraded = append(r.upgraded, d) }
func (r *recordedMetrics) ComponentReady(c string, d time.Duration) {
	if r.ready == nil {
		r.ready = map[string]time.Duration{}
	}
	r.ready[c] = d
}
func (r *recordedMetrics) ResultWriteFailed(kind string) { r.results = append(r.results, kind) }

func TestMeasureObservesOnlyTheTransitions(t *testing.T) {
	at := time.Unix(1800000000, 0)
	row := &model.Cluster{CreatedAt: at.Add(-25 * time.Minute)}
	provisioning := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING}
	running := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING}
	updating := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_UPDATING,
		Upgrade: &cloudv1.ClusterUpgrade{StartTime: timestamppb.New(at.Add(-40 * time.Minute))}}
	imported := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING, Import: &cloudv1.ImportStatus{}}
	installing := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING, PlatformComponents: []*cloudv1.PlatformComponent{
		{Name: "karpenter", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, Since: timestamppb.New(at.Add(-3 * time.Minute))},
		{Name: "cert-manager", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, Since: timestamppb.New(at.Add(-time.Minute))}}}
	ready := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING, PlatformComponents: []*cloudv1.PlatformComponent{
		{Name: "karpenter", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY},
		{Name: "cert-manager", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED}}}
	cases := []struct {
		name                  string
		before, after         *cloudv1.ClusterStatus
		provisioned, upgraded int
		ready                 map[string]time.Duration
	}{
		{"first running", provisioning, running, 1, 0, nil},
		{"running again after an update", &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_UPDATING}, running, 0, 0, nil},
		{"import is not a creation", provisioning, imported, 0, 0, nil},
		{"upgrade finished", updating, running, 0, 1, nil},
		{"unchanged", running, running, 0, 0, nil},
		{"component ready", installing, ready, 0, 0, map[string]time.Duration{"karpenter": 3 * time.Minute}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &recordedMetrics{}
			measure(m, row, tc.before, tc.after, at)
			if len(m.provisioned) != tc.provisioned || len(m.upgraded) != tc.upgraded || len(m.ready) != len(tc.ready) {
				t.Fatalf("got %+v", m)
			}
			if tc.provisioned == 1 && m.provisioned[0] != 25*time.Minute {
				t.Fatalf("provisioning %v", m.provisioned[0])
			}
			if tc.upgraded == 1 && m.upgraded[0] != 40*time.Minute {
				t.Fatalf("upgrade %v", m.upgraded[0])
			}
			for name, d := range tc.ready {
				if m.ready[name] != d {
					t.Fatalf("%s %v", name, m.ready[name])
				}
			}
		})
	}
}
