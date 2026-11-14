package cli

import cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"

// maxTableMessage is how much of a message a table row shows; the whole
// error follows the table.
const maxTableMessage = 100

// oneLine is s for a table cell: on one line, cut at maxTableMessage.
func oneLine(s string) string {
	out := make([]rune, 0, maxTableMessage)
	for _, r := range s {
		if r == '\n' || r == '\t' || r == '\r' {
			r = ' '
		}
		if len(out) == maxTableMessage {
			return string(out) + "..."
		}
		out = append(out, r)
	}
	return string(out)
}

func componentPhaseWord(p cloudv1.ComponentPhase) string {
	switch p {
	case cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING:
		return "Pending"
	case cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING:
		return "Installing"
	case cloudv1.ComponentPhase_COMPONENT_PHASE_READY:
		return "Ready"
	case cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED:
		return "Failed"
	default:
		return "Unknown"
	}
}
