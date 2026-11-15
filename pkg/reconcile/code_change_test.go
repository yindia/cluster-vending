package reconcile

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var codeAt = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

const (
	pendingPhase  = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING
	applyingPhase = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING
	donePhase     = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE
	haltedPhase   = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_HALTED
)

// What a pass does about a code change: adopt the first fingerprints;
// nothing when the render is what was applied; wait while frozen or not
// admitted; start when admitted; go on once started; stay halted until a
// retry. A renderer version bump is a code change too.
func TestDecideCode(t *testing.T) {
	applied := map[string]string{"a": "1", "b": "2"}
	changed := map[string]string{"a": "1", "b": "3", "c": "4"}
	halted := &cloudv1.CodeChange{Phase: haltedPhase, Failures: 4, Message: "applying it failed 4 times", Target: sequence.Digest(changed)}
	newer := map[string]string{"a": "9", "b": "3", "c": "4"}
	applying := &cloudv1.CodeChange{Phase: applyingPhase, Failures: 1}
	tests := []struct {
		name                  string
		f                     CodeFacts
		phase                 cloudv1.CodeChangePhase
		adopt, apply, started bool
		message               string
	}{
		{"first fingerprints", CodeFacts{Now: changed}, 0, true, false, false, ""},
		{"nothing changed", CodeFacts{Applied: applied, Now: applied}, 0, false, false, false, ""},
		{"applied meanwhile", CodeFacts{Applied: applied, Now: applied, Prev: &cloudv1.CodeChange{Phase: pendingPhase, Message: "waiting"}},
			donePhase, false, false, false, ""},
		{"frozen", CodeFacts{Applied: applied, Now: changed, Frozen: true, FrozenWhy: "the platform is frozen: deploy", Admitted: true},
			pendingPhase, false, false, false, "the platform is frozen: deploy"},
		{"not admitted", CodeFacts{Applied: applied, Now: changed, NotAdmitted: "waiting its turn"}, pendingPhase, false, false, false, "waiting its turn"},
		{"admitted", CodeFacts{Applied: applied, Now: changed, Admitted: true}, applyingPhase, false, true, true, ""},
		{"renderer behind", CodeFacts{Now: changed, Behind: true, Admitted: true}, applyingPhase, false, true, true, ""},
		{"applying goes on, frozen or not", CodeFacts{Applied: applied, Now: changed, Prev: applying, Frozen: true}, applyingPhase, false, true, false, ""},
		{"halted", CodeFacts{Applied: applied, Now: changed, Prev: halted, Admitted: true}, haltedPhase, false, false, false, "failed 4 times"},
		{"halted, retried", CodeFacts{Applied: applied, Now: changed, Prev: halted, Admitted: true, Retry: true}, applyingPhase, false, true, true, ""},
		{"halted, a newer build", CodeFacts{Applied: applied, Now: newer, Prev: halted, Admitted: true}, applyingPhase, false, true, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.f.At = codeAt

			d := DecideCode(tt.f)

			if d.Status.GetPhase() != tt.phase || d.Adopt != tt.adopt || d.Apply != tt.apply || d.Started != tt.started ||
				!strings.Contains(d.Status.GetMessage(), tt.message) {
				t.Errorf("decision %+v (status %v)", d, d.Status)
			}
		})
	}
	if d := DecideCode(CodeFacts{Applied: applied, Now: changed, Admitted: true, At: codeAt}); d.Status.GetSummary() != "2 objects change (1 added)" ||
		d.Status.GetObjects() != 2 || !d.Status.GetStartTime().AsTime().Equal(codeAt) {
		t.Errorf("summary %v", d.Status)
	}
}

// An apply that completes finishes the change; one that fails is tried
// again until the recoveries are used up, then halts; one held back keeps
// applying.
func TestCodeApplied(t *testing.T) {
	st := &cloudv1.CodeChange{Phase: applyingPhase, Summary: "1 object changes"}

	done, note := CodeApplied(st, true, nil, codeAt)
	if done.GetPhase() != donePhase || done.GetFinishTime() == nil || note == nil || note.Reason != ReasonCodeChangeFinished {
		t.Errorf("complete: %v %v", done, note)
	}
	if held, note := CodeApplied(st, false, nil, codeAt); held.GetPhase() != applyingPhase || note != nil {
		t.Errorf("held back: %v %v", held, note)
	}
	failing := st
	for i := 1; i <= CodeRecoveries; i++ {
		var note *Note
		failing, note = CodeApplied(failing, false, errors.New("boom"), codeAt)
		if failing.GetPhase() != applyingPhase || note != nil || failing.GetFailures() != int32(i) {
			t.Fatalf("failure %d: %v %v", i, failing, note)
		}
	}
	last, note := CodeApplied(failing, false, errors.New("boom"), codeAt)
	if last.GetPhase() != haltedPhase || note == nil || note.Reason != ReasonCodeChangeHalted || note.Type != EventWarning {
		t.Errorf("past the recoveries: %v %v", last, note)
	}
	if st.GetFailures() != 0 {
		t.Error("the status given was changed")
	}
}
