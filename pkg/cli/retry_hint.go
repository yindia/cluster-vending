package cli

import (
	"fmt"
	"io"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// retryHint is what to do about a failed component: try its chart again
// (cluster retry), or, when it failed before it had a chart (a
// precondition: IRSA off, a requirement missing), fix what its message
// says; it then installs by itself. "" when it has not failed.
func retryHint(clusterID, name string, phase cloudv1.ComponentPhase, chart, message string) string {
	switch {
	case phase != cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED:
		return ""
	case chart == "":
		return "fix first, then it installs by itself: " + oneLine(message)
	default:
		return fmt.Sprintf("retry: cluster-vending cluster retry --id %s --component %s", clusterID, name)
	}
}

// recoverySection is cluster get's list of how to recover each failed
// platform component; nothing when none failed.
func recoverySection(w io.Writer, clusterID string, components []*cloudv1.PlatformComponent) error {
	var lines []string
	for _, p := range components {
		if hint := retryHint(clusterID, p.GetName(), p.GetPhase(), p.GetChart(), p.GetMessage()); hint != "" {
			lines = append(lines, p.GetName()+": "+hint)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	if _, err := fmt.Fprint(w, "\nRecovery:\n"); err != nil {
		return err
	}
	for _, l := range lines {
		if _, err := fmt.Fprintf(w, "  %s\n", l); err != nil {
			return err
		}
	}
	return nil
}
