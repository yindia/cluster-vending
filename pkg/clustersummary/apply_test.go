package clustersummary

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func fixtureSummary(at time.Time, requests bool) cloudv1alpha1.ClusterSummary {
	a := NewAggregator()
	a.AddNode("system", "n1", true, alloc("2", "4Gi", 29))
	a.AddNode("pool-a", "n2", true, alloc("4", "8Gi", 58))
	a.AddPod(pod("n1", "Running", ctr("100m", "128Mi")))
	a.AddPod(pod("n2", "Running", ctr("250m", "256Mi")))
	return a.Summary(at, requests)
}

func statusWithGroups() *cloudv1alpha1.ClusterStatus {
	return &cloudv1alpha1.ClusterStatus{NodeGroups: []cloudv1alpha1.NodeGroupStatus{
		{Name: "system", PlatformManaged: true, Phase: cloudv1alpha1.NodeGroupPhaseReady, Nodes: 1},
		{Name: "pool-a", Phase: cloudv1alpha1.NodeGroupPhaseReady, Nodes: 1},
		{Name: "empty", Phase: cloudv1alpha1.NodeGroupPhasePending},
	}}
}

func TestApplyWritesTotalsGroupsAndReachable(t *testing.T) {
	st := statusWithGroups()
	if !Apply(st, fixtureSummary(t0, true)) {
		t.Fatal("expected a change")
	}
	if st.Nodes.Total != 2 || st.Nodes.Ready != 2 {
		t.Fatalf("nodes = %+v", st.Nodes)
	}
	if g := st.NodeGroups[0]; g.AllocatableCPUMillis != 2000 || g.RequestedCPUMillis != 100 || g.PodCount != 1 || g.Phase != cloudv1alpha1.NodeGroupPhaseReady {
		t.Fatalf("system = %+v", g)
	}
	if g := st.NodeGroups[2]; g.AllocatableCPUMillis != 0 || g.PodCount != 0 {
		t.Fatalf("a group with no nodes has zero usage: %+v", g)
	}
	c := meta.FindStatusCondition(st.Conditions, cloudv1alpha1.ConditionReachable)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("reachable = %+v", c)
	}
}

func TestApplyUnreachableKeepsCounts(t *testing.T) {
	st := statusWithGroups()
	Apply(st, fixtureSummary(t0, true))
	before := st.DeepCopy()
	changed := Apply(st, cloudv1alpha1.ClusterSummary{Reachable: false, Reason: cloudv1alpha1.ReasonClusterUnreachable, At: metav1.NewTime(t0.Add(time.Minute))})
	if !changed {
		t.Fatal("the condition changed")
	}
	c := meta.FindStatusCondition(st.Conditions, cloudv1alpha1.ConditionReachable)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != cloudv1alpha1.ReasonClusterUnreachable {
		t.Fatalf("reachable = %+v", c)
	}
	if c.Message == "" {
		t.Fatal("the message should say when the cluster was last read")
	}
	if !equality.Semantic.DeepEqual(st.Nodes, before.Nodes) || len(st.NodeGroups) != len(before.NodeGroups) {
		t.Fatalf("counts changed: %+v", st.Nodes)
	}
	for i := range st.NodeGroups {
		if st.NodeGroups[i] != before.NodeGroups[i] {
			t.Fatalf("group %d changed: %+v", i, st.NodeGroups[i])
		}
	}
	// Reachable again flips the condition back.
	Apply(st, fixtureSummary(t0.Add(2*time.Minute), true))
	if c := meta.FindStatusCondition(st.Conditions, cloudv1alpha1.ConditionReachable); c.Status != metav1.ConditionTrue {
		t.Fatalf("reachable = %+v", c)
	}
}

func TestApplyUnreachableWithoutAnyCountsWritesNone(t *testing.T) {
	st := statusWithGroups()
	Apply(st, cloudv1alpha1.ClusterSummary{Reason: cloudv1alpha1.ReasonCredentialsRejected, At: metav1.NewTime(t0)})
	if st.Nodes != nil {
		t.Fatalf("nodes = %+v", st.Nodes)
	}
	c := meta.FindStatusCondition(st.Conditions, cloudv1alpha1.ConditionReachable)
	if c == nil || c.Reason != cloudv1alpha1.ReasonCredentialsRejected {
		t.Fatalf("reachable = %+v", c)
	}
}

func TestApplyTimeOnlyRefreshAtMostEveryTenMinutes(t *testing.T) {
	st := statusWithGroups()
	Apply(st, fixtureSummary(t0, true))
	if Apply(st, fixtureSummary(t0.Add(5*time.Minute), true)) {
		t.Fatal("unchanged counts 5 minutes later must write nothing")
	}
	if !st.Nodes.NodesCollectedAt.Time.Equal(t0) {
		t.Fatalf("collected at moved: %v", st.Nodes.NodesCollectedAt)
	}
	if !Apply(st, fixtureSummary(t0.Add(10*time.Minute), true)) {
		t.Fatal("unchanged counts 10 minutes later refresh the time")
	}
	if !st.Nodes.NodesCollectedAt.Time.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("collected at = %v", st.Nodes.NodesCollectedAt)
	}
	// A changed count writes at once.
	next := fixtureSummary(t0.Add(11*time.Minute), true)
	next.Nodes.Total++
	if !Apply(st, next) {
		t.Fatal("changed counts write at once")
	}
}

func TestApplyKeepsRequestedWhenPodsWereNotRead(t *testing.T) {
	st := statusWithGroups()
	Apply(st, fixtureSummary(t0, true))
	next := fixtureSummary(t0.Add(time.Minute), false)
	for k, g := range next.NodeGroups { // what a failed pod list would have left
		g.RequestedCPUMillis, g.PodCount = 0, 0
		next.NodeGroups[k] = g
	}
	next.NodeGroups["system"] = func() cloudv1alpha1.NodeGroupUsage {
		g := next.NodeGroups["system"]
		g.AllocatableCPUMillis = 3000
		return g
	}()
	Apply(st, next)
	g := st.NodeGroups[0]
	if g.AllocatableCPUMillis != 3000 || g.RequestedCPUMillis != 100 || g.PodCount != 1 {
		t.Fatalf("system = %+v", g)
	}
}

func TestApplyAppendsOtherEntryAndStaysBounded(t *testing.T) {
	a := NewAggregator()
	st := &cloudv1alpha1.ClusterStatus{}
	for i := 0; i < 30; i++ {
		name := string(rune('a'+i%26)) + string(rune('a'+i/26))
		a.AddNode(name, name, true, alloc("1", "1Gi", 10))
		st.NodeGroups = append(st.NodeGroups, cloudv1alpha1.NodeGroupStatus{Name: name})
	}
	Apply(st, a.Summary(t0, true))
	var other *cloudv1alpha1.NodeGroupStatus
	withUsage := 0
	for i := range st.NodeGroups {
		g := &st.NodeGroups[i]
		if g.Name == cloudv1alpha1.StatusOtherNodeGroup {
			other = g
		}
		if g.AllocatableCPUMillis > 0 {
			withUsage++
		}
	}
	if other == nil || other.Ready != 15 {
		t.Fatalf("other = %+v", other)
	}
	if withUsage != MaxGroups {
		t.Fatalf("%d entries carry usage, want %d", withUsage, MaxGroups)
	}
}

func TestKeepUsageSurvivesAStatusRebuilt(t *testing.T) {
	old := statusWithGroups()
	Apply(old, fixtureSummary(t0, true))
	rebuilt := &cloudv1alpha1.ClusterStatus{NodeGroups: []cloudv1alpha1.NodeGroupStatus{
		{Name: "system", Nodes: 5}, {Name: "pool-a"}, {Name: "new"}}}
	Keep(rebuilt, old)
	if g := rebuilt.NodeGroups[0]; g.Nodes != 5 || g.AllocatableCPUMillis != 2000 {
		t.Fatalf("system = %+v", g)
	}
	if rebuilt.Nodes == nil || meta.FindStatusCondition(rebuilt.Conditions, cloudv1alpha1.ConditionReachable) == nil {
		t.Fatal("nodes and the Reachable condition are kept")
	}
	if rebuilt.NodeGroups[2].AllocatableCPUMillis != 0 {
		t.Fatal("a new group has no usage yet")
	}
}

// On a first pass the status lists no groups yet: the summary's groups are
// added, with their node counts.
func TestApplyAddsGroupsTheStatusDoesNotListYet(t *testing.T) {
	st := &cloudv1alpha1.ClusterStatus{}
	Apply(st, fixtureSummary(t0, true))
	if len(st.NodeGroups) != 2 || st.NodeGroups[0].Name != "pool-a" || st.NodeGroups[0].Nodes != 1 || st.NodeGroups[0].RequestedCPUMillis != 250 {
		t.Fatalf("groups = %+v", st.NodeGroups)
	}
}
