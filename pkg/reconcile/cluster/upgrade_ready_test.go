package cluster

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func readyRow(t *testing.T, platform ...*cloudv1.PlatformComponent) *model.Cluster {
	t.Helper()
	st, err := protojson.Marshal(&cloudv1.ClusterStatus{PlatformComponents: platform})
	if err != nil {
		t.Fatal(err)
	}
	return &model.Cluster{Resource: model.Resource{State: StateRunning, Status: st}}
}

func tenantComponent(t *testing.T, name, state string, phase cloudv1.ComponentPhase) model.Component {
	t.Helper()
	st, err := protojson.Marshal(&cloudv1.ComponentStatus{Phase: phase})
	if err != nil {
		t.Fatal(err)
	}
	return model.Component{Component: name, State: state, Status: st}
}

// An automatic upgrade starts only once EKS add-ons (the cluster is
// RUNNING: every resource Ready), every platform component and every
// enabled tenant component are Ready.
func TestUpgradeHold(t *testing.T) {
	readyPC := &cloudv1.PlatformComponent{Name: "karpenter", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY}
	tests := []struct {
		name       string
		row        *model.Cluster
		components []model.Component
		want       string
	}{
		{name: "everything ready", row: readyRow(t, readyPC),
			components: []model.Component{tenantComponent(t, "grafana", "", cloudv1.ComponentPhase_COMPONENT_PHASE_READY)}},
		{name: "no components at all", row: readyRow(t)},
		{name: "add-ons or resources not ready", row: func() *model.Cluster { r := readyRow(t, readyPC); r.State = StateUpdating; return r }(),
			want: "the cluster is not RUNNING (its EKS add-ons and resources are not all Ready)"},
		{name: "platform component installing", row: readyRow(t, readyPC,
			&cloudv1.PlatformComponent{Name: "kyverno", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING}),
			want: "platform component kyverno is not Ready"},
		{name: "platform component being removed", row: readyRow(t, readyPC,
			&cloudv1.PlatformComponent{Name: "kyverno", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING})},
		{name: "tenant component pending", row: readyRow(t, readyPC),
			components: []model.Component{tenantComponent(t, "grafana", "", cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING)},
			want:       "tenant component grafana is not Ready"},
		{name: "tenant component being removed", row: readyRow(t, readyPC),
			components: []model.Component{tenantComponent(t, "grafana", componentDeleting, cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UpgradeHold(tt.row, tt.components); got != tt.want {
				t.Errorf("UpgradeHold = %q, want %q", got, tt.want)
			}
		})
	}
}
