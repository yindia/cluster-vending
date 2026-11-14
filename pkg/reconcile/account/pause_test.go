package account

import (
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// A paused account says so in its status, like a network or a cluster:
// status.pause with who, since when and why, and the Paused condition.
func TestProtoOfAPausedAccount(t *testing.T) {
	since := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		pause *model.Pause
		want  string // the Paused condition's message; "" for none
	}{
		{"not paused", nil, ""},
		{"paused", &model.Pause{By: "alice", Since: since, Reason: "provider upgrade"},
			"paused by alice since 2026-11-14T09:00:00Z (provider upgrade): nothing is created, changed or deleted until it is resumed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &model.CloudAccount{TenantID: "acme", Namespace: "tenant-acme", Paused: tt.pause}

			st := Proto(a).GetStatus()

			var paused *cloudv1.Condition
			for _, c := range st.GetConditions() {
				if c.GetType() == reconcile.ConditionPaused {
					paused = c
				}
			}
			if tt.want == "" {
				if st.GetPause() != nil || paused != nil {
					t.Fatalf("an account that is not paused: pause %v, condition %v", st.GetPause(), paused)
				}
				return
			}
			if st.GetPause().GetBy() != "alice" || !st.GetPause().GetSince().AsTime().Equal(since) || st.GetPause().GetReason() != "provider upgrade" {
				t.Errorf("status.pause = %v", st.GetPause())
			}
			if paused == nil || paused.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE || !strings.HasPrefix(paused.GetMessage(), tt.want) {
				t.Errorf("Paused condition = %v, want a message starting %q", paused, tt.want)
			}
		})
	}
}
