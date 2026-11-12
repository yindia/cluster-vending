package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A wait for an import ends when the plan is there, not when the call
// returned: the call only stored the import.
func TestImportObservation(t *testing.T) {
	phase := func(p cloudv1.ImportPhase) *cloudv1.ImportStatus { return &cloudv1.ImportStatus{Phase: p} }
	for name, tt := range map[string]struct {
		imp          *cloudv1.ImportStatus
		resources    int
		failed       bool
		done, isFail bool
	}{
		"stored, not looked up yet":       {phase(cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING), 0, false, false, false},
		"the lookup is retried":           {phase(cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING), 0, false, false, false},
		"what to import does not exist":   {phase(cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING), 0, true, false, true},
		"planned, objects not there yet":  {phase(cloudv1.ImportPhase_IMPORT_PHASE_PLANNED), 0, false, false, false},
		"planned and observed":            {phase(cloudv1.ImportPhase_IMPORT_PHASE_PLANNED), 12, false, true, false},
		"planned, an object cannot apply": {phase(cloudv1.ImportPhase_IMPORT_PHASE_PLANNED), 3, true, false, true},
		"blocked: the plan is the answer": {phase(cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED), 0, true, true, false},
		"no import status at all":         {nil, 0, false, false, false},
	} {
		got := importObservation("STATE", "message", tt.imp, tt.resources, tt.failed)
		if got.done != tt.done || got.failed != tt.isFail || got.state != "STATE" || got.message != "message" {
			t.Errorf("%s: done %t, failed %t; want done %t, failed %t", name, got.done, got.failed, tt.done, tt.isFail)
		}
	}
}

// A resource that turns FAILED ends the wait with its message as the
// error (a non-zero exit); one that is done ends it without.
func TestWaitUntilEndsOnFailedWithTheMessage(t *testing.T) {
	var progress strings.Builder
	o := Options{Timeout: time.Second, Progress: &progress}
	err := waitUntil(t.Context(), o, "cluster c-1", false, func(context.Context) (observation, error) {
		return observation{state: "CLUSTER_STATE_FAILED", message: "EKS has a cluster of this name", failed: true}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "CLUSTER_STATE_FAILED") || !strings.Contains(err.Error(), "EKS has a cluster of this name") {
		t.Errorf("a failed resource = %v, want an error with its state and message", err)
	}
	if !strings.Contains(progress.String(), "cluster c-1: CLUSTER_STATE_FAILED") {
		t.Errorf("progress = %q", progress.String())
	}
	err = waitUntil(t.Context(), o, "network n-1", false, func(context.Context) (observation, error) {
		return observation{state: "NETWORK_STATE_IMPORTED", done: true}, nil
	})
	if err != nil {
		t.Errorf("a done resource = %v", err)
	}
}
