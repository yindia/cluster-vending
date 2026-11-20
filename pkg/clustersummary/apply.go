package clustersummary

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// refreshInterval is how old the collected time gets before a pass whose
// counts did not change moves it.
const refreshInterval = 10 * time.Minute

// Apply puts what a pass learned onto the status and reports whether anything
// changed. An unreachable pass sets only the Reachable condition: the counts
// stay, and their CollectedAt times show their age. A pass whose counts equal
// the recorded ones moves the collected time at most every refreshInterval.
func Apply(st *cloudv1alpha1.ClusterStatus, sum cloudv1alpha1.ClusterSummary) bool {
	before := st.DeepCopy()
	setReachable(st, sum)
	if sum.Reachable && sum.Nodes != nil {
		applyNodes(st, sum)
	}
	return !equality.Semantic.DeepEqual(before, st)
}

func setReachable(st *cloudv1alpha1.ClusterStatus, sum cloudv1alpha1.ClusterSummary) {
	c := metav1.Condition{Type: cloudv1alpha1.ConditionReachable, Status: metav1.ConditionTrue, Reason: "Reachable",
		Message: "the controller read the workload cluster"}
	if !sum.Reachable {
		c.Status, c.Reason = metav1.ConditionFalse, sum.Reason
		if c.Reason == "" {
			c.Reason = "Unreachable"
		}
		c.Message = "the workload cluster cannot be read"
		if st.Nodes != nil && st.Nodes.NodesCollectedAt != nil {
			c.Message = fmt.Sprintf("the workload cluster cannot be read; showing the counts of %s", st.Nodes.NodesCollectedAt.UTC().Format(time.RFC3339))
		}
	}
	// A condition that did not change keeps its time: only the transition moves it.
	if cur := meta.FindStatusCondition(st.Conditions, c.Type); cur != nil && cur.Status == c.Status && cur.Reason == c.Reason && cur.Message == c.Message {
		return
	}
	c.LastTransitionTime = sum.At
	meta.SetStatusCondition(&st.Conditions, c)
}

func applyNodes(st *cloudv1alpha1.ClusterStatus, sum cloudv1alpha1.ClusterSummary) {
	next := *sum.Nodes
	groups := make([]cloudv1alpha1.NodeGroupStatus, len(st.NodeGroups))
	copy(groups, st.NodeGroups)
	seen := map[string]bool{}
	for i := range groups {
		g := &groups[i]
		u, ok := sum.NodeGroups[g.Name]
		seen[g.Name] = ok
		g.NodeGroupUsage = merge(g.NodeGroupUsage, u, sum.RequestsKnown)
	}
	// A group the status does not list yet (the contract status is written
	// after the summary on a first pass) or the "other" entry gets one.
	for _, name := range sortedNames(sum.NodeGroups) {
		if !seen[name] {
			u := sum.NodeGroups[name]
			groups = append(groups, cloudv1alpha1.NodeGroupStatus{Name: name, Nodes: u.Ready + u.NotReady,
				NodeGroupUsage: merge(cloudv1alpha1.NodeGroupUsage{}, u, sum.RequestsKnown)})
		}
	}
	// Unchanged counts refresh the time only now and then.
	if cur := st.Nodes; cur != nil && cur.NodesCollectedAt != nil && next.NodesCollectedAt != nil {
		same := *cur
		same.NodesCollectedAt = next.NodesCollectedAt
		if same == next && equality.Semantic.DeepEqual(groups, st.NodeGroups) && next.NodesCollectedAt.Sub(cur.NodesCollectedAt.Time) < refreshInterval {
			return
		}
	}
	st.Nodes = &next
	st.NodeGroups = groups
}

// merge is the group's usage after a pass: the pass's numbers, the recorded
// requested ones kept when the pods were not read.
func merge(recorded, read cloudv1alpha1.NodeGroupUsage, requestsKnown bool) cloudv1alpha1.NodeGroupUsage {
	if !requestsKnown {
		read.RequestedCPUMillis, read.RequestedMemoryBytes = recorded.RequestedCPUMillis, recorded.RequestedMemoryBytes
		read.PodCount, read.PodsWithoutRequests = recorded.PodCount, recorded.PodsWithoutRequests
	}
	return read
}

func sortedNames(m map[string]cloudv1alpha1.NodeGroupUsage) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	// Few entries (at most MaxGroups): insertion order does not matter, but a
	// stable one keeps the status stable.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Keep copies onto a status rebuilt from the contract status what the contract
// does not carry: the summary fields, the groups' usage (by name) and the
// Reachable condition. Rebuilding the status must not lose them.
func Keep(into, from *cloudv1alpha1.ClusterStatus) {
	into.Nodes = from.Nodes.DeepCopy()
	into.Violations = from.Violations.DeepCopy()
	into.ViolationEvents = from.ViolationEvents.DeepCopy()
	if c := meta.FindStatusCondition(from.Conditions, cloudv1alpha1.ConditionReachable); c != nil {
		meta.SetStatusCondition(&into.Conditions, *c)
	}
	usage := map[string]cloudv1alpha1.NodeGroupUsage{}
	for _, g := range from.NodeGroups {
		usage[g.Name] = g.NodeGroupUsage
	}
	for i := range into.NodeGroups {
		into.NodeGroups[i].NodeGroupUsage = usage[into.NodeGroups[i].Name]
	}
	// The "other" entry is not a group the contract lists.
	for _, g := range from.NodeGroups {
		if g.Name == cloudv1alpha1.StatusOtherNodeGroup {
			into.NodeGroups = append(into.NodeGroups, g)
		}
	}
}
