package clustersummary

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

func alloc(cpu, mem string, pods int64) corev1.ResourceList {
	l := rl(cpu, mem)
	l[corev1.ResourcePods] = *resource.NewQuantity(pods, resource.DecimalSI)
	return l
}

func pod(node string, phase corev1.PodPhase, cs ...corev1.Container) *corev1.Pod {
	return &corev1.Pod{Spec: corev1.PodSpec{NodeName: node, Containers: cs}, Status: corev1.PodStatus{Phase: phase}}
}

// Three nodes in two groups and 40 pods; the expected numbers are summed by
// hand (what `kubectl describe node` adds up).
func TestAggregatorThreeNodesFortyPods(t *testing.T) {
	const gi = 1 << 30
	a := NewAggregator()
	a.AddNode("system", "n1", true, alloc("2", "4Gi", 29))
	a.AddNode("pool-a", "n2", true, alloc("4", "8Gi", 58))
	a.AddNode("pool-a", "n3", false, alloc("4", "8Gi", 58))

	// n1: 10 running pods of 100m/128Mi, one with an init container peak of 500m.
	for i := 0; i < 9; i++ {
		a.AddPod(pod("n1", corev1.PodRunning, ctr("100m", "128Mi")))
	}
	big := pod("n1", corev1.PodRunning, ctr("100m", "128Mi"))
	big.Spec.InitContainers = []corev1.Container{ctr("500m", "64Mi")}
	a.AddPod(big)
	// n2: 20 pods of 250m/256Mi, plus overhead 10m/8Mi on one; 3 finished pods; one unbound pod.
	for i := 0; i < 20; i++ {
		a.AddPod(pod("n2", corev1.PodRunning, ctr("250m", "256Mi")))
	}
	for i := 0; i < 2; i++ {
		a.AddPod(pod("n2", corev1.PodSucceeded, ctr("1", "1Gi")))
	}
	a.AddPod(pod("n2", corev1.PodFailed, ctr("1", "1Gi")))
	a.AddPod(pod("", corev1.PodPending, ctr("1", "1Gi")))
	// n3: 8 pods without memory requests, 2 with none at all.
	for i := 0; i < 8; i++ {
		a.AddPod(pod("n3", corev1.PodRunning, ctr("50m", "")))
	}
	a.AddPod(pod("n3", corev1.PodRunning, ctr("", "")))
	a.AddPod(pod("n3", corev1.PodRunning, ctr("", "")))
	// A pod on a node outside the groups is not counted.
	a.AddPod(pod("elsewhere", corev1.PodRunning, ctr("1", "1Gi")))

	s := a.Summary(time.Unix(1000, 0), true)
	want := map[string]cloudv1alpha1.NodeGroupUsage{
		// 9*100m + (max(100m, 500m)=500m) = 1400m; 9*128Mi + max(128Mi,64Mi)=128Mi -> 10*128Mi.
		"system": {Ready: 1, AllocatableCPUMillis: 2000, AllocatableMemoryBytes: 4 * gi, AllocatablePods: 29,
			RequestedCPUMillis: 1400, RequestedMemoryBytes: 10 * 128 << 20, PodCount: 10},
		// n2: 20*250m = 5000m, 20*256Mi; n3: 8*50m = 400m, 0 memory; 10 pods, 10 without requests.
		"pool-a": {Ready: 1, NotReady: 1, AllocatableCPUMillis: 8000, AllocatableMemoryBytes: 16 * gi, AllocatablePods: 116,
			RequestedCPUMillis: 5400, RequestedMemoryBytes: 20 * 256 << 20, PodCount: 30, PodsWithoutRequests: 10},
	}
	if len(s.NodeGroups) != len(want) {
		t.Fatalf("groups = %v", s.NodeGroups)
	}
	for name, w := range want {
		if got := s.NodeGroups[name]; got != w {
			t.Errorf("%s: got %+v, want %+v", name, got, w)
		}
	}
	if n := s.Nodes; n.Total != 3 || n.Ready != 2 || n.NotReady != 1 {
		t.Errorf("nodes = %+v", n)
	}
	if !s.RequestsKnown {
		t.Error("requests should be known")
	}
}

func TestAggregatorNoPods(t *testing.T) {
	a := NewAggregator()
	a.AddNode("system", "n1", true, alloc("2", "4Gi", 29))
	g := a.Summary(time.Unix(1, 0), true).NodeGroups["system"]
	if g.RequestedCPUMillis != 0 || g.RequestedMemoryBytes != 0 || g.PodCount != 0 || g.AllocatableCPUMillis != 2000 {
		t.Fatalf("got %+v", g)
	}
}

func TestAggregatorNoNodes(t *testing.T) {
	s := NewAggregator().Summary(time.Unix(1, 0), false)
	if s.Nodes.Total != 0 || len(s.NodeGroups) != 0 || s.RequestsKnown {
		t.Fatalf("got %+v", s)
	}
}

func TestOverflowGroupsAreSummedIntoOther(t *testing.T) {
	a := NewAggregator()
	for i := 0; i < 20; i++ {
		a.AddNode(fmt.Sprintf("g%02d", i), fmt.Sprintf("n%d", i), true, alloc("1", "1Gi", 10))
	}
	s := a.Summary(time.Unix(1, 0), true)
	if len(s.NodeGroups) != MaxGroups {
		t.Fatalf("got %d groups, want %d", len(s.NodeGroups), MaxGroups)
	}
	other := s.NodeGroups[cloudv1alpha1.StatusOtherNodeGroup]
	if other.Ready != 5 || other.AllocatableCPUMillis != 5000 {
		t.Fatalf("other = %+v", other)
	}
	if s.Nodes.Total != 20 {
		t.Fatalf("total = %d", s.Nodes.Total)
	}
}

// The status a 1,000-node cluster with 16 groups carries stays under 8 KiB.
func TestSummarySizeBound(t *testing.T) {
	a := NewAggregator()
	for i := 0; i < 1000; i++ {
		a.AddNode(fmt.Sprintf("a-rather-long-node-group-name-%02d", i%40), fmt.Sprintf("n%d", i), i%7 != 0, alloc("96", "384Gi", 737))
	}
	s := a.Summary(time.Now(), true)
	st := cloudv1alpha1.ClusterStatus{Nodes: s.Nodes}
	for name, u := range s.NodeGroups {
		st.NodeGroups = append(st.NodeGroups, cloudv1alpha1.NodeGroupStatus{
			Name: name, PlatformManaged: true, Phase: cloudv1alpha1.NodeGroupPhaseReady,
			Message: string(make([]rune, 0)), Nodes: 999, NodeGroupUsage: u})
	}
	now := metav1.Now()
	st.Nodes.NodesCollectedAt = &now
	snap := manyViolations(500, 200)
	st.Violations, st.ViolationEvents = ViolationCounts(snap, time.Now())
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.NodeGroups) != MaxGroups {
		t.Fatalf("groups = %d", len(st.NodeGroups))
	}
	if len(raw) >= 8<<10 {
		t.Fatalf("summary is %d bytes, want < 8192", len(raw))
	}
	t.Logf("summary is %d bytes", len(raw))
}
