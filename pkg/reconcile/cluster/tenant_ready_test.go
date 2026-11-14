package cluster

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// TenantComponentsReady is told once, when the last of the cluster's
// enabled tenant components becomes Ready; not again while they stay so.
func TestTenantComponentsReadyNote(t *testing.T) {
	ready, installing := cloudv1.ComponentPhase_COMPONENT_PHASE_READY, cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING
	tests := []struct {
		name          string
		before, after []model.Component
		want          bool
	}{
		{"the last one became ready", comps(t, ready, installing), comps(t, ready, ready), true},
		{"they were ready already", comps(t, ready, ready), comps(t, ready, ready), false},
		{"one is still installing", comps(t, installing, installing), comps(t, ready, installing), false},
		{"none enabled", nil, nil, false},
		{"the first one, enabled and ready in one pass", nil, comps(t, ready), true},
		{"one being removed does not count", comps(t, installing), append(comps(t, ready), deleting(t)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, ok := tenantComponentsReadyNote(tt.before, tt.after)

			if ok != tt.want {
				t.Fatalf("note = %v (%v), want %v", n, ok, tt.want)
			}
			if ok && (n.Reason != reconcile.ReasonTenantComponentsReady || n.Type != reconcile.Normal("", "").Type) {
				t.Errorf("note = %+v", n)
			}
		})
	}
}

func comps(t *testing.T, phases ...cloudv1.ComponentPhase) []model.Component {
	t.Helper()
	out := make([]model.Component, 0, len(phases))
	for i, p := range phases {
		c := model.Component{ID: string(rune('a' + i)), Component: "c" + string(rune('a'+i))}
		setTenantPhase(t, &c, p)
		out = append(out, c)
	}
	return out
}

func deleting(t *testing.T) model.Component {
	t.Helper()
	c := model.Component{ID: "z", Component: "gone", State: componentDeleting}
	setTenantPhase(t, &c, cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING)
	return c
}

func setTenantPhase(t *testing.T, c *model.Component, p cloudv1.ComponentPhase) {
	t.Helper()
	raw, err := protojson.Marshal(&cloudv1.ComponentStatus{Phase: p})
	if err != nil {
		t.Fatal(err)
	}
	c.Status = raw
}
