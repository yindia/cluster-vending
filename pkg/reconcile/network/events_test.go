package network

import (
	"slices"
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestStatusEvents(t *testing.T) {
	const (
		provisioning = cloudv1.NetworkState_NETWORK_STATE_PROVISIONING
		ready        = cloudv1.NetworkState_NETWORK_STATE_READY
		updating     = cloudv1.NetworkState_NETWORK_STATE_UPDATING
		failed       = cloudv1.NetworkState_NETWORK_STATE_FAILED
		deleting     = cloudv1.NetworkState_NETWORK_STATE_DELETING
		importedSt   = cloudv1.NetworkState_NETWORK_STATE_IMPORTED
	)
	st := func(state cloudv1.NetworkState, message string) *cloudv1.NetworkStatus {
		return &cloudv1.NetworkStatus{State: state, Message: message,
			ManagedResources: []*cloudv1.ManagedResource{{Kind: "VPC", Name: "v"}, {Kind: "Subnet", Name: "s"}}}
	}
	imported := func(phase cloudv1.ImportPhase, state cloudv1.NetworkState) *cloudv1.NetworkStatus {
		return &cloudv1.NetworkStatus{State: state, Import: &cloudv1.ImportStatus{Phase: phase,
			Resources: []*cloudv1.ImportedResource{{Disposition: cloudv1.Disposition_DISPOSITION_BLOCKING, Detail: "a peering connection"}}}}
	}
	passFailed := func() *cloudv1.NetworkStatus {
		s := st(provisioning, "")
		withFailure(s, &cloudv1.Condition{Type: ConditionReconciled, Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
			Reason: ReasonFailed, Message: "the cloud account is not bootstrapped"})
		return s
	}
	tests := []struct {
		name          string
		before, after *cloudv1.NetworkStatus
		want          []string // "Type Reason"
		message       string   // part of the last event's message
	}{
		{name: "nothing recorded, nothing found", before: &cloudv1.NetworkStatus{}, after: &cloudv1.NetworkStatus{}},
		{name: "the first pass", before: &cloudv1.NetworkStatus{}, after: st(provisioning, "0 of 2 resources ready"),
			want: []string{"Normal Provisioning"}},
		{name: "still provisioning", before: st(provisioning, "0 of 2 resources ready"), after: st(provisioning, "1 of 2 resources ready")},
		{name: "ready", before: st(provisioning, ""), after: st(ready, ""), want: []string{"Normal Ready"}, message: "all 2 resources"},
		{name: "ready stays ready", before: st(ready, ""), after: st(ready, "")},
		{name: "a change is applied", before: st(ready, ""), after: st(updating, "1 of 2 resources ready"),
			want: []string{"Normal Updating"}, message: "1 of 2"},
		{name: "a resource fails", before: st(ready, ""), after: st(failed, "NATGateway n: quota exceeded"),
			want: []string{"Warning Failed"}, message: "NATGateway n"},
		{name: "the same resource fails with another text", before: st(failed, "NATGateway n: try 1"), after: st(failed, "NATGateway n: try 2")},
		{name: "the pass fails", before: st(provisioning, ""), after: passFailed(), want: []string{"Warning Failed"},
			message: "the cloud account is not bootstrapped"},
		{name: "the pass fails again the same way", before: passFailed(), after: passFailed()},
		{name: "deletion starts", before: st(ready, ""), after: st(deleting, "deleting NATGateway; 9 resources left"),
			want: []string{"Normal Deleting"}, message: "NATGateway"},
		{name: "deletion goes on", before: st(deleting, "deleting NATGateway; 9 resources left"),
			after: st(deleting, "deleting NATGateway; 8 resources left")},
		{name: "the next tier is deleted", before: st(deleting, "deleting NATGateway; 8 resources left"),
			after: st(deleting, "deleting Subnet; 5 resources left"), want: []string{"Normal Deleting"}, message: "Subnet"},
		{name: "an import is discovered", before: imported(cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING, importedSt),
			after: imported(cloudv1.ImportPhase_IMPORT_PHASE_PLANNED, importedSt), want: []string{"Normal ImportDiscovered"}},
		{name: "an import is blocked", before: imported(cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING, importedSt),
			after: imported(cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED, failed), want: []string{"Warning ImportBlocked"},
			message: "a peering connection"},
		{name: "an import is taken over", before: imported(cloudv1.ImportPhase_IMPORT_PHASE_PLANNED, importedSt),
			after: imported(cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING, updating), want: []string{"Normal TakenOver"}},
		{name: "a taken-over network is ready", before: imported(cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING, updating),
			after: imported(cloudv1.ImportPhase_IMPORT_PHASE_CONVERGED, ready), want: []string{"Normal Ready"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := statusEvents(tt.before, tt.after)

			var have []string
			for _, n := range got {
				have = append(have, n.Type+" "+n.Reason)
			}
			if !slices.Equal(have, tt.want) {
				t.Fatalf("events = %v, want %v", have, tt.want)
			}
			if tt.message != "" && !strings.Contains(got[len(got)-1].Message, tt.message) {
				t.Errorf("message %q lacks %q", got[len(got)-1].Message, tt.message)
			}
		})
	}
}
