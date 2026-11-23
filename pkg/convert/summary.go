package convert

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// SummaryFromAPI is the cluster status summary the kind holds (PROP-02 5.2)
// as the contract's ClusterStatus carries it: nodes, violations, violation
// events, reachable, and the node groups by name with their resource totals
// only. Nil when the kind holds none of it (no pass has read the cluster):
// a summary is absent then, never zeros.
func SummaryFromAPI(st *cloudv1alpha1.ClusterStatus) *cloudv1.ClusterStatus {
	if st == nil {
		return nil
	}
	out := &cloudv1.ClusterStatus{}
	if n := st.Nodes; n != nil {
		out.Nodes = &cloudv1.ClusterNodeCounts{Total: n.Total, Ready: n.Ready, NotReady: n.NotReady,
			NodesCollectedAt: timestampOf(n.NodesCollectedAt)}
	}
	if v := st.Violations; v != nil {
		out.Violations = &cloudv1.ClusterViolationCounts{Total: v.Total, BySeverity: countsFromAPI(v.BySeverity),
			PolicyCollectedAt: timestampOf(v.PolicyCollectedAt)}
	}
	if e := st.ViolationEvents; e != nil {
		out.ViolationEvents = &cloudv1.ClusterViolationEventCounts{Total: e.Total, ByReason: countsFromAPI(e.ByReason),
			PolicyCollectedAt: timestampOf(e.PolicyCollectedAt)}
	}
	if c := meta.FindStatusCondition(st.Conditions, cloudv1alpha1.ConditionReachable); c != nil {
		r := &cloudv1.ClusterReachability{Status: conditionStatusFromAPI(c.Status), Message: c.Message}
		if c.Status != metav1.ConditionTrue {
			r.Reason = c.Reason
		}
		if st.Nodes != nil {
			r.LastSuccessfulAt = timestampOf(st.Nodes.NodesCollectedAt)
		}
		out.Reachable = r
	}
	for _, g := range st.NodeGroups {
		if g.NodeGroupUsage == (cloudv1alpha1.NodeGroupUsage{}) {
			continue
		}
		out.NodeGroups = append(out.NodeGroups, withUsage(&cloudv1.NodeGroupStatus{Name: g.Name, Nodes: g.Nodes}, g.NodeGroupUsage))
	}
	if out.Nodes == nil && out.Violations == nil && out.ViolationEvents == nil && out.Reachable == nil && len(out.NodeGroups) == 0 {
		return nil
	}
	return out
}

// WithSummary puts the summary sum (SummaryFromAPI) onto st: its four
// fields, and each node group's resource totals by name. A group the
// summary has and st does not list (the "other" entry, or a group the
// contract status does not name yet) is appended. A nil sum changes nothing.
func WithSummary(st, sum *cloudv1.ClusterStatus) {
	if st == nil || sum == nil {
		return
	}
	st.Nodes, st.Violations, st.ViolationEvents, st.Reachable = sum.GetNodes(), sum.GetViolations(), sum.GetViolationEvents(), sum.GetReachable()
	index := make(map[string]*cloudv1.NodeGroupStatus, len(st.NodeGroups))
	for _, g := range st.NodeGroups {
		index[g.GetName()] = g
	}
	for _, u := range sum.GetNodeGroups() {
		g, ok := index[u.GetName()]
		if !ok {
			st.NodeGroups = append(st.NodeGroups, u)
			continue
		}
		g.ReadyNodes, g.NotReadyNodes = u.GetReadyNodes(), u.GetNotReadyNodes()
		g.AllocatableCpuMillis, g.AllocatableMemoryBytes, g.AllocatablePods = u.GetAllocatableCpuMillis(), u.GetAllocatableMemoryBytes(), u.GetAllocatablePods()
		g.RequestedCpuMillis, g.RequestedMemoryBytes = u.GetRequestedCpuMillis(), u.GetRequestedMemoryBytes()
		g.PodCount, g.PodsWithoutRequests = u.GetPodCount(), u.GetPodsWithoutRequests()
	}
}

func withUsage(g *cloudv1.NodeGroupStatus, u cloudv1alpha1.NodeGroupUsage) *cloudv1.NodeGroupStatus {
	g.ReadyNodes, g.NotReadyNodes = u.Ready, u.NotReady
	g.AllocatableCpuMillis, g.AllocatableMemoryBytes, g.AllocatablePods = u.AllocatableCPUMillis, u.AllocatableMemoryBytes, u.AllocatablePods
	g.RequestedCpuMillis, g.RequestedMemoryBytes = u.RequestedCPUMillis, u.RequestedMemoryBytes
	g.PodCount, g.PodsWithoutRequests = u.PodCount, u.PodsWithoutRequests
	return g
}

func countsFromAPI(in []cloudv1alpha1.ClusterCount) []*cloudv1.ClusterCount {
	if len(in) == 0 {
		return nil
	}
	out := make([]*cloudv1.ClusterCount, 0, len(in))
	for _, c := range in {
		out = append(out, &cloudv1.ClusterCount{Name: c.Name, Count: c.Count})
	}
	return out
}
