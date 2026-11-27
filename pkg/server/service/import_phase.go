package service

import (
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ImportNotPlanned is the FAILED_PRECONDITION of a takeover whose import
// plan is not ready: the phase in words, what to wait for, and, while the
// lookup fails (lookupError, the resource's status.message when its state
// is FAILED), why. noun is "cluster" or "network", the CLI's group.
func ImportNotPlanned(noun, id string, imp *cloudv1.ImportStatus, lookupError string) error {
	check := fmt.Sprintf("watch status.import with %s get %s", noun, id)
	var msg string
	switch imp.GetPhase() {
	case cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING, cloudv1.ImportPhase_IMPORT_PHASE_UNSPECIFIED:
		msg = fmt.Sprintf("%s %s cannot be taken over yet: the controller is still looking up its resources and there is no import plan; "+
			"wait until status.import.phase is PLANNED (%s), review the plan, then take over", noun, id, check)
		if lookupError != "" {
			msg += "; the lookup is failing: " + oneLine(lookupError)
		}
	case cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED:
		msg = fmt.Sprintf("%s %s cannot be taken over: its import is blocked by resources the platform cannot express", noun, id)
		if first := firstBlocking(imp); first != nil {
			msg += fmt.Sprintf(" (first: %s %s: %s)", first.GetKind(), first.GetExternalName(), first.GetDetail())
		}
		msg += "; " + check + " for the BLOCKING lines"
	default: // CONVERGING, CONVERGED
		msg = fmt.Sprintf("%s %s was already taken over (status.import.phase is %s); %s",
			noun, id, phaseWord(imp.GetPhase()), check)
	}
	return connect.NewError(connect.CodeFailedPrecondition, msg)
}

func firstBlocking(imp *cloudv1.ImportStatus) *cloudv1.ImportedResource {
	for _, r := range imp.GetResources() {
		if r.GetDisposition() == cloudv1.Disposition_DISPOSITION_BLOCKING {
			return r
		}
	}
	return nil
}

// phaseWord is IMPORT_PHASE_CONVERGING as CONVERGING.
func phaseWord(p cloudv1.ImportPhase) string {
	return strings.TrimPrefix(p.String(), "IMPORT_PHASE_")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
