package cli

import (
	"fmt"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// codeChangeLine is a code change (a new controller build rendering the
// objects differently) in one line: what changes and where it is in its
// rollout, "3 objects change: waiting its turn in the channel's code
// rollout: tier 2 first". Empty when none is pending.
func codeChangeLine(c *cloudv1.CodeChange) string {
	what := c.GetSummary()
	if what == "" {
		what = "a code change"
	}
	switch c.GetPhase() {
	case cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING:
		if c.GetMessage() == "" {
			return what + ": waiting its turn"
		}
		return what + ": " + oneLine(c.GetMessage())
	case cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING:
		line := what + ": applying since " + age(c.GetStartTime())
		if c.GetFailures() > 0 {
			line += " (" + oneLine(c.GetMessage()) + ")"
		}
		return line
	case cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_HALTED:
		return what + ": halted: " + oneLine(c.GetMessage())
	case cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE, cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_UNSPECIFIED:
	}
	return ""
}

// catalogChangeLine is a catalog change (a platform definition or the
// platform pool changed) in one line: what changes, and where it is in
// the code rollout and on the cluster. Empty when none is pending.
func catalogChangeLine(c *cloudv1.CatalogChange) string {
	what := c.GetSummary()
	if what == "" {
		what = "a catalog change"
	}
	switch c.GetPhase() {
	case cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING:
		if c.GetMessage() == "" {
			return what + ": waiting its turn"
		}
		return what + ": " + oneLine(c.GetMessage())
	case cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING:
		return fmt.Sprintf("%s: step %d of %d, %s", what, c.GetStep(), c.GetSteps(), oneLine(c.GetMessage()))
	case cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_HALTED:
		return what + ": halted: " + oneLine(c.GetMessage())
	case cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE, cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_UNSPECIFIED:
	}
	return ""
}
