package reconcile

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A code change that applied but never converges halts once it has been
// applying for CodeConvergenceTimeout, with a warning that says why; one
// still inside the bound, or not applying, is left alone.
func TestCodeStalled(t *testing.T) {
	start := codeAt.Add(-CodeConvergenceTimeout - 1)
	applying := &cloudv1.CodeChange{Phase: applyingPhase, Summary: "1 changed", StartTime: timestamppb.New(start)}

	out, note := CodeStalled(applying, "VPC vpc is not Ready", codeAt)
	if out.GetPhase() != haltedPhase || note == nil || note.Reason != ReasonCodeChangeHalted || note.Type != EventWarning {
		t.Fatalf("stalled change = %v, note %+v; want halted with a warning", out, note)
	}
	if !strings.Contains(out.GetMessage(), "did not converge") || !strings.Contains(out.GetMessage(), "VPC vpc is not Ready") {
		t.Fatalf("message %q does not say why", out.GetMessage())
	}
	if applying.GetPhase() != applyingPhase {
		t.Fatal("CodeStalled changed its input")
	}

	recent := &cloudv1.CodeChange{Phase: applyingPhase, StartTime: timestamppb.New(codeAt.Add(-CodeConvergenceTimeout + 1))}
	if out, note := CodeStalled(recent, "x", codeAt); out.GetPhase() != applyingPhase || note != nil {
		t.Fatalf("change inside the bound = %v, %+v; want unchanged", out, note)
	}
	done := &cloudv1.CodeChange{Phase: donePhase, StartTime: timestamppb.New(start)}
	if out, note := CodeStalled(done, "x", codeAt); out.GetPhase() != donePhase || note != nil {
		t.Fatalf("done change = %v, %+v; want unchanged", out, note)
	}
	if out, note := CodeStalled(&cloudv1.CodeChange{Phase: applyingPhase}, "x", codeAt); out.GetPhase() != applyingPhase || note != nil {
		t.Fatalf("change without a start = %v, %+v; want unchanged", out, note)
	}
}
