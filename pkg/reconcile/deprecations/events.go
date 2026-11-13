package deprecations

import (
	"fmt"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Event types and reasons of what a scan reports (the same words as
// pkg/reconcile's, which this package does not import).
const (
	eventNormal  = "Normal"
	eventWarning = "Warning"

	ReasonDeprecatedAPIs = "DeprecatedAPIs"
	ReasonScanFailed     = "DeprecationScanFailed"
	ReasonScanClean      = "DeprecationScanClean"
)

// Event is what a scan has to report about its cluster.
type Event struct{ Type, Reason, Message string }

// scanEvent says what a scan's result means next to the one before it:
// API versions removed at the target were found (when their number or the
// target changed), the scan failed (when the one before did not), or a
// scan that had findings or had failed found nothing removed now. Nil
// when the result says what the last one did.
func scanEvent(last, result *cloudv1.ClusterDeprecations) *Event {
	sameTarget := last.GetTargetVersion() == result.GetTargetVersion()
	switch {
	case result.GetError() != "":
		if last.GetError() != "" {
			return nil // still failing: said once
		}
		return &Event{eventWarning, ReasonScanFailed, "the scan for deprecated API versions failed: " + result.GetError()}
	case result.GetRemoved() > 0:
		if last != nil && last.GetError() == "" && sameTarget && last.GetRemoved() == result.GetRemoved() {
			return nil
		}
		return &Event{eventWarning, ReasonDeprecatedAPIs, fmt.Sprintf(
			"%d objects use API versions that Kubernetes %s removes (%d deprecated in all); see the cluster's deprecations",
			result.GetRemoved(), result.GetTargetVersion(), result.GetTotal())}
	case last != nil && (last.GetError() != "" || last.GetRemoved() > 0):
		return &Event{eventNormal, ReasonScanClean, fmt.Sprintf(
			"no object uses an API version that Kubernetes %s removes", result.GetTargetVersion())}
	default:
		return nil
	}
}
