package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The ages after which the summary's parts are old (PROP-02 5.2): the node
// counts after 15 minutes, the policy counts after 30.
const (
	nodesOldAfter  = 15 * time.Minute
	policyOldAfter = 30 * time.Minute
)

// notReadYet is what describe prints for a part of the cluster status
// summary no controller pass has read: never zeros.
const notReadYet = "not read yet"

// summaryLines are describe's lines of the cluster status summary: whether
// the platform can read the cluster, the node counts, and the violation
// counts with the policy events of the last hour (one policy pass reads
// both), each with its age.
func summaryLines(st *cloudv1.ClusterStatus) [][2]string {
	return [][2]string{
		{"Reachable", reachableLine(st.GetReachable())},
		{"Nodes", nodesLine(st.GetNodes())},
		{"Violations", violationsLine(st.GetViolations(), st.GetViolationEvents())},
	}
}

func reachableLine(r *cloudv1.ClusterReachability) string {
	switch r.GetStatus() {
	case cloudv1.ConditionStatus_CONDITION_STATUS_TRUE:
		return "yes"
	case cloudv1.ConditionStatus_CONDITION_STATUS_FALSE:
		line := "no"
		if r.GetReason() != "" {
			line += " (" + r.GetReason() + ")"
		}
		if t := r.GetLastSuccessfulAt(); t != nil {
			line += "; showing the counts of " + age(t) + " ago"
		}
		return line
	default:
		return notReadYet
	}
}

func nodesLine(n *cloudv1.ClusterNodeCounts) string {
	if n == nil {
		return notReadYet
	}
	return fmt.Sprintf("%d (%d ready, %d not ready), %s", n.GetTotal(), n.GetReady(), n.GetNotReady(),
		collected(n.GetNodesCollectedAt(), nodesOldAfter))
}

func violationsLine(v *cloudv1.ClusterViolationCounts, e *cloudv1.ClusterViolationEventCounts) string {
	if v == nil && e == nil {
		return notReadYet
	}
	var parts []string
	at := v.GetPolicyCollectedAt()
	if v != nil {
		parts = append(parts, strconv.Itoa(int(v.GetTotal()))+countsText(v.GetBySeverity()))
	}
	if e != nil {
		parts = append(parts, fmt.Sprintf("%d policy events in the last hour%s", e.GetTotal(), countsText(e.GetByReason())))
		if at == nil {
			at = e.GetPolicyCollectedAt()
		}
	}
	return strings.Join(parts, "; ") + ", " + collected(at, policyOldAfter)
}

// countsText is " (high 3, low 1)"; "" for none.
func countsText(in []*cloudv1.ClusterCount) string {
	if len(in) == 0 {
		return ""
	}
	parts := make([]string, 0, len(in))
	for _, c := range in {
		parts = append(parts, c.GetName()+" "+strconv.Itoa(int(c.GetCount())))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// collected is "collected 4m ago", with " (old)" past oldAfter.
func collected(t *timestamppb.Timestamp, oldAfter time.Duration) string {
	if t == nil {
		return "collected " + none
	}
	line := "collected " + age(t) + " ago"
	if clock().Sub(t.AsTime()) > oldAfter {
		line += " (old)"
	}
	return line
}

// groupRead: the node pass recorded the group's resource totals.
func groupRead(g *cloudv1.NodeGroupStatus) bool {
	return g.GetAllocatableCpuMillis() != 0 || g.GetAllocatableMemoryBytes() != 0 || g.GetReadyNodes() != 0 ||
		g.GetNotReadyNodes() != 0 || g.GetPodCount() != 0
}
