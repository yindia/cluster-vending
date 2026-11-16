package reconcile

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestUnchangedNetworkEpochIsObservationWithoutApplyOrNewSoak(t *testing.T) {
	finished := timestamppb.New(time.Now().Add(-48 * time.Hour))
	fingerprints := map[string]string{"vpc": "same"}
	prev := &cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE, Target: "old/render", FinishTime: finished}
	facts := CodeFacts{Epoch: "current", Applied: fingerprints, Now: fingerprints, Prev: prev, At: time.Now()}
	if facts.Pending() {
		t.Fatal("unchanged restart became a real code change")
	}
	decision := DecideCode(facts)
	if decision.Apply || decision.Started || decision.Adopt || decision.Status.Target != facts.Target() || !decision.Status.FinishTime.AsTime().Equal(finished.AsTime()) {
		t.Fatalf("restart reapplies or resets soak: %+v", decision)
	}
	if prev.Target != "old/render" {
		t.Fatal("observation mutated persisted status input")
	}
}

func TestNetworkConvergenceRemainsInFlightAfterSSA(t *testing.T) {
	fingerprints := map[string]string{"vpc": "applied"}
	facts := CodeFacts{Epoch: "current", Applied: fingerprints, Now: fingerprints, Prev: &cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING}, WaitForConvergence: true, At: time.Now()}
	decision := DecideCode(facts)
	if !decision.Apply || decision.Started || decision.Status.Phase != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING {
		t.Fatalf("desired SSA incorrectly credited as cloud convergence: %+v", decision)
	}
}
