package clustersummary

import (
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// MaxGroups is the most node group entries a summary carries; groups beyond
// MaxGroups-1 are summed into one "other" entry.
const MaxGroups = 16

// Aggregator sums nodes and pods per node group. Nodes are added first, then
// pods (each page as it arrives; a pod is never kept). Pods on a node that was
// not added are ignored.
type Aggregator struct {
	groups map[string]*cloudv1alpha1.NodeGroupUsage
	// group of each node, by node name.
	pods map[string]string
	// nodes counted per readiness.
	total, ready int32
}

// NewAggregator is an empty Aggregator.
func NewAggregator() *Aggregator {
	return &Aggregator{groups: map[string]*cloudv1alpha1.NodeGroupUsage{}, pods: map[string]string{}}
}

func (a *Aggregator) group(name string) *cloudv1alpha1.NodeGroupUsage {
	g := a.groups[name]
	if g == nil {
		g = &cloudv1alpha1.NodeGroupUsage{}
		a.groups[name] = g
	}
	return g
}

// AddNode counts a node of group with its allocatable resources.
func (a *Aggregator) AddNode(group, name string, ready bool, allocatable corev1.ResourceList) {
	g := a.group(group)
	a.pods[name] = group
	a.total++
	if ready {
		g.Ready++
		a.ready++
	} else {
		g.NotReady++
	}
	g.AllocatableCPUMillis += allocatable.Cpu().MilliValue()
	g.AllocatableMemoryBytes += allocatable.Memory().Value()
	g.AllocatablePods += allocatable.Pods().Value()
}

// AddNodeObject is AddNode for a Node as the API returned it, unstructured.
func (a *Aggregator) AddNodeObject(group string, n *unstructured.Unstructured) {
	var node corev1.Node
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(n.Object, &node); err != nil {
		a.AddNode(group, n.GetName(), false, nil)
		return
	}
	ready := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	a.AddNode(group, node.Name, ready, node.Status.Allocatable)
}

// AddPod counts the pod against its node's group. A finished pod, an unbound
// one and one on an unknown node are ignored.
func (a *Aggregator) AddPod(p *corev1.Pod) {
	if !Counted(p) {
		return
	}
	group, ok := a.pods[p.Spec.NodeName]
	if !ok {
		return
	}
	g := a.group(group)
	r := PodRequests(p)
	g.RequestedCPUMillis += r.CPUMillis
	g.RequestedMemoryBytes += r.MemoryBytes
	g.PodCount++
	if r.MissingRequests {
		g.PodsWithoutRequests++
	}
}

// AddPodObject is AddPod for a Pod as the API returned it, unstructured. A
// pod that cannot be decoded is ignored.
func (a *Aggregator) AddPodObject(u *unstructured.Unstructured) {
	var p corev1.Pod
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p); err != nil {
		return
	}
	a.AddPod(&p)
}

// Summary is what the pass records. requestsKnown: the pod list was read to
// the end, so the requested numbers are current.
func (a *Aggregator) Summary(at time.Time, requestsKnown bool) cloudv1alpha1.ClusterSummary {
	t := metav1.NewTime(at.UTC())
	out := cloudv1alpha1.ClusterSummary{
		Reachable: true, At: t, RequestsKnown: requestsKnown,
		Nodes: &cloudv1alpha1.ClusterNodesStatus{Total: a.total, Ready: a.ready, NotReady: a.total - a.ready, NodesCollectedAt: &t},
	}
	names := make([]string, 0, len(a.groups))
	for n := range a.groups {
		names = append(names, n)
	}
	slices.SortFunc(names, strings.Compare)
	out.NodeGroups = map[string]cloudv1alpha1.NodeGroupUsage{}
	other := cloudv1alpha1.NodeGroupUsage{}
	for i, n := range names {
		if len(names) > MaxGroups && i >= MaxGroups-1 {
			addUsage(&other, *a.groups[n])
			continue
		}
		out.NodeGroups[n] = *a.groups[n]
	}
	if len(names) > MaxGroups {
		out.NodeGroups[cloudv1alpha1.StatusOtherNodeGroup] = other
	}
	return out
}

func addUsage(into *cloudv1alpha1.NodeGroupUsage, u cloudv1alpha1.NodeGroupUsage) {
	into.Ready += u.Ready
	into.NotReady += u.NotReady
	into.AllocatableCPUMillis += u.AllocatableCPUMillis
	into.AllocatableMemoryBytes += u.AllocatableMemoryBytes
	into.AllocatablePods += u.AllocatablePods
	into.RequestedCPUMillis += u.RequestedCPUMillis
	into.RequestedMemoryBytes += u.RequestedMemoryBytes
	into.PodCount += u.PodCount
	into.PodsWithoutRequests += u.PodsWithoutRequests
}
