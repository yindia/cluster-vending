package clustersummary

import (
	"fmt"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// NodesStaleAfter is how old the node part of the summary may get before the
// console marks it old and the upgrade readiness gates wait (PROP-02 5.2,
// open question 8).
const NodesStaleAfter = 15 * time.Minute

// PolicyStaleAfter is how old the policy counts may get before the console
// marks them old: two missed counts passes.
const PolicyStaleAfter = 30 * time.Minute

// Wait is why an upgrade must wait on the cluster's summary (sum, as
// convert.SummaryFromAPI gives it) at now: the platform cannot read the
// cluster, or the node counts are older than NodesStaleAfter. "" when it
// need not. A cluster without a summary (no pass has read it yet) does not
// wait here: the other gates decide, and the first pass records one.
func Wait(sum *cloudv1.ClusterStatus, now time.Time) string {
	if sum == nil {
		return ""
	}
	if r := sum.GetReachable(); r != nil && r.GetStatus() == cloudv1.ConditionStatus_CONDITION_STATUS_FALSE {
		return "the platform cannot read the cluster"
	}
	at := sum.GetNodes().GetNodesCollectedAt()
	if at == nil {
		return ""
	}
	if age := now.Sub(at.AsTime()); age > NodesStaleAfter {
		return fmt.Sprintf("the cluster's summary is %d minutes old", int(age/time.Minute))
	}
	return ""
}
