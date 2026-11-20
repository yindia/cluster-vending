package cluster

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var summaryAt = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// pool0 is replaced by the default node group's name when the nodes are created.
const pool0 = "<system>"

func summaryNode(name, pool string, ready bool, cpu, mem string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if pool == "" {
		n.Labels[managedNodeGroupLabel] = pool0
	} else {
		n.Labels[crossplaneaws.KarpenterNodePoolLabel] = pool
	}
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}}
	n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem), corev1.ResourcePods: resource.MustParse("58")}
	return n
}

func summaryPod(name, node string, phase corev1.PodPhase, cpu, mem string) *corev1.Pod {
	req := corev1.ResourceList{}
	if cpu != "" {
		req[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		req[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:   corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: req}}}},
		Status: corev1.PodStatus{Phase: phase}}
}

// fortyPods: 3 nodes; the sums are worked out by hand in the test.
func fortyPods() []client.Object {
	objs := []client.Object{
		summaryNode("n1", "", true, "2", "4Gi"),
		summaryNode("n2", "platform", true, "4", "8Gi"),
		summaryNode("n3", "platform", false, "4", "8Gi"),
	}
	for i := 0; i < 10; i++ { // n1: 10 x 100m/128Mi
		objs = append(objs, summaryPod(fmt.Sprintf("a%d", i), "n1", corev1.PodRunning, "100m", "128Mi"))
	}
	for i := 0; i < 20; i++ { // n2: 20 x 250m/256Mi
		objs = append(objs, summaryPod(fmt.Sprintf("b%d", i), "n2", corev1.PodRunning, "250m", "256Mi"))
	}
	for i := 0; i < 6; i++ { // n3: 6 x 50m, no memory request
		objs = append(objs, summaryPod(fmt.Sprintf("c%d", i), "n3", corev1.PodRunning, "50m", ""))
	}
	// 4 more that do not count: finished (2), unbound, and on an unknown node.
	objs = append(objs,
		summaryPod("d0", "n2", corev1.PodSucceeded, "1", "1Gi"), summaryPod("d1", "n2", corev1.PodFailed, "1", "1Gi"),
		summaryPod("d2", "", corev1.PodPending, "1", "1Gi"), summaryPod("d3", "gone", corev1.PodRunning, "1", "1Gi"))
	return objs
}

// summaryWorld is a node group world whose workload cluster has the objects;
// the default managed group's nodes get the group's real name.
func summaryWorld(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) (world, *model.Cluster, *fakeWorkload) {
	t.Helper()
	fw := newFakeWorkload(funcs)
	w, row := nodeGroupWorld(t, fw)
	w.row = row
	systemGroup := crossplaneaws.DefaultNodeGroupName(crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name))
	for _, o := range objs {
		if n, ok := o.(*corev1.Node); ok && n.Labels[managedNodeGroupLabel] == pool0 {
			n.Labels[managedNodeGroupLabel] = systemGroup
		}
		require.NoError(t, fw.c.Create(t.Context(), o))
	}
	return w, row, fw
}

func clusterStatusOf(t *testing.T, w world) cloudv1alpha1.ClusterStatus {
	t.Helper()
	obj := &cloudv1alpha1.Cluster{}
	require.NoError(t, w.storeKube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: w.row.Name}, obj))
	return obj.Status
}

func groupOf(st cloudv1alpha1.ClusterStatus, name string) cloudv1alpha1.NodeGroupStatus {
	for _, g := range st.NodeGroups {
		if g.Name == name {
			return g
		}
	}
	return cloudv1alpha1.NodeGroupStatus{}
}

func setNow(t *testing.T, at time.Time) {
	t.Helper()
	was := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = was })
}

func TestPassRecordsGroupTotalsOfThreeNodesAndFortyPods(t *testing.T) {
	setNow(t, summaryAt)
	const mi = 1 << 20
	w, row, _ := summaryWorld(t, interceptor.Funcs{}, fortyPods()...)

	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	st := clusterStatusOf(t, w)
	require.NotNil(t, st.Nodes)
	require.Equal(t, int32(3), st.Nodes.Total)
	require.Equal(t, int32(2), st.Nodes.Ready)
	require.Equal(t, int32(1), st.Nodes.NotReady)
	require.Equal(t, summaryAt, st.Nodes.NodesCollectedAt.UTC())
	// system (n1): 10 x 100m = 1000m, 10 x 128Mi; allocatable 2 CPU, 4Gi.
	sys := groupOf(st, systemNodeGroup)
	require.Equal(t, int64(2000), sys.AllocatableCPUMillis)
	require.Equal(t, int64(4<<30), sys.AllocatableMemoryBytes)
	require.Equal(t, int64(58), sys.AllocatablePods)
	require.Equal(t, int64(1000), sys.RequestedCPUMillis)
	require.Equal(t, int64(10*128*mi), sys.RequestedMemoryBytes)
	require.Equal(t, int32(10), sys.PodCount)
	require.Equal(t, int32(0), sys.PodsWithoutRequests)
	// platform (n2, n3): 20 x 250m + 6 x 50m = 5300m; 20 x 256Mi; 26 pods, 6 without memory requests.
	pf := groupOf(st, "platform")
	require.Equal(t, int64(8000), pf.AllocatableCPUMillis)
	require.Equal(t, int64(16<<30), pf.AllocatableMemoryBytes)
	require.Equal(t, int64(5300), pf.RequestedCPUMillis)
	require.Equal(t, int64(20*256*mi), pf.RequestedMemoryBytes)
	require.Equal(t, int32(26), pf.PodCount)
	require.Equal(t, int32(6), pf.PodsWithoutRequests)
	require.Equal(t, int32(1), pf.Ready)
	require.Equal(t, int32(1), pf.NotReady)
	c := meta.FindStatusCondition(st.Conditions, cloudv1alpha1.ConditionReachable)
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionTrue, c.Status)
}

func TestPassWithNoPodsReportsZeroRequests(t *testing.T) {
	setNow(t, summaryAt)
	w, row, _ := summaryWorld(t, interceptor.Funcs{}, summaryNode("n1", "", true, "2", "4Gi"))

	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	sys := groupOf(clusterStatusOf(t, w), systemNodeGroup)
	require.Equal(t, int64(2000), sys.AllocatableCPUMillis)
	require.Zero(t, sys.RequestedCPUMillis)
	require.Zero(t, sys.PodCount)
}

// The pods are listed once per pass, in pages of at most 500 with continue,
// and only a page is held at a time.
func TestPodsAreListedOncePerPassInPages(t *testing.T) {
	setNow(t, summaryAt)
	pageSizes := []int64{}
	total := 1200
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		u, ok := list.(*unstructured.UnstructuredList)
		if !ok || u.GetKind() != "PodList" {
			return c.List(ctx, list, opts...)
		}
		lo := &client.ListOptions{}
		lo.ApplyOptions(opts)
		pageSizes = append(pageSizes, lo.Limit)
		start := 0
		if lo.Continue != "" {
			start, _ = strconv.Atoi(lo.Continue)
		}
		end := min(start+int(lo.Limit), total)
		for i := start; i < end; i++ {
			p := unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": fmt.Sprintf("p%d", i), "namespace": "default"},
				"spec": map[string]any{"nodeName": "n1", "containers": []any{map[string]any{"name": "c",
					"resources": map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "1Mi"}}}}},
				"status": map[string]any{"phase": "Running"}}}
			u.Items = append(u.Items, p)
		}
		if end < total {
			u.SetContinue(strconv.Itoa(end))
		}
		return nil
	}}
	w, row, _ := summaryWorld(t, funcs, summaryNode("n1", "", true, "96", "384Gi"))

	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	require.Equal(t, []int64{500, 500, 500}, pageSizes)
	sys := groupOf(clusterStatusOf(t, w), systemNodeGroup)
	require.Equal(t, int32(1200), sys.PodCount)
	require.Equal(t, int64(12000), sys.RequestedCPUMillis)
}

// Passes within a minute of each other read the pods and write the summary
// once.
func TestSummaryIsThrottledToOnceAMinute(t *testing.T) {
	setNow(t, summaryAt)
	podLists := 0
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == "PodList" {
			podLists++
		}
		return c.List(ctx, list, opts...)
	}}
	w, row, _ := summaryWorld(t, funcs, summaryNode("n1", "", true, "2", "4Gi"))

	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	setNow(t, summaryAt.Add(20*time.Second))
	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	require.Equal(t, 1, podLists)
	setNow(t, summaryAt.Add(61*time.Second))
	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	require.Equal(t, 2, podLists)
}

// A pod list that fails keeps the recorded requests; the nodes' numbers move.
func TestFailedPodListKeepsRecordedRequests(t *testing.T) {
	setNow(t, summaryAt)
	fail := false
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == "PodList" && fail {
			return errors.New("etcd timeout")
		}
		return c.List(ctx, list, opts...)
	}}
	w, row, _ := summaryWorld(t, funcs, fortyPods()...)
	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	fail = true
	setNow(t, summaryAt.Add(2*time.Minute))

	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	sys := groupOf(clusterStatusOf(t, w), systemNodeGroup)
	require.Equal(t, int32(10), sys.PodCount)
	require.Equal(t, int64(1000), sys.RequestedCPUMillis)
}

// An unreachable workload cluster sets Reachable False and keeps the counts.
func TestUnreachableWorkloadClusterKeepsTheLastCounts(t *testing.T) {
	setNow(t, summaryAt)
	w, row, fw := summaryWorld(t, interceptor.Funcs{}, fortyPods()...)
	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	fw.err = errors.New("dial tcp: i/o timeout")
	setNow(t, summaryAt.Add(5*time.Minute))
	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	st := clusterStatusOf(t, w)
	c := meta.FindStatusCondition(st.Conditions, cloudv1alpha1.ConditionReachable)
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionFalse, c.Status)
	require.Equal(t, cloudv1alpha1.ReasonClusterUnreachable, c.Reason)
	require.Equal(t, int32(3), st.Nodes.Total)
	require.Equal(t, summaryAt, st.Nodes.NodesCollectedAt.UTC(), "the counts show their age")
	require.Equal(t, int32(26), groupOf(st, "platform").PodCount)

	// Back again.
	fw.err = nil
	setNow(t, summaryAt.Add(10*time.Minute))
	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	c = meta.FindStatusCondition(clusterStatusOf(t, w).Conditions, cloudv1alpha1.ConditionReachable)
	require.Equal(t, metav1.ConditionTrue, c.Status)
}

func TestRejectedCredentialsAreTold(t *testing.T) {
	setNow(t, summaryAt)
	w, row, fw := summaryWorld(t, interceptor.Funcs{}, fortyPods()...)
	fw.err = apierrors.NewUnauthorized("token expired")

	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	c := meta.FindStatusCondition(clusterStatusOf(t, w).Conditions, cloudv1alpha1.ConditionReachable)
	require.NotNil(t, c)
	require.Equal(t, cloudv1alpha1.ReasonCredentialsRejected, c.Reason)
}

// A node list the cluster refuses mid-pass is an unreachable cluster too.
func TestNodeListForbiddenIsRejectedCredentials(t *testing.T) {
	setNow(t, summaryAt)
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind() == (schema.GroupVersionKind{Version: "v1", Kind: "NodeList"}) {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("no"))
		}
		return c.List(ctx, list, opts...)
	}}
	w, row, _ := summaryWorld(t, funcs, fortyPods()...)

	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	st := clusterStatusOf(t, w)
	c := meta.FindStatusCondition(st.Conditions, cloudv1alpha1.ConditionReachable)
	require.NotNil(t, c)
	require.Equal(t, cloudv1alpha1.ReasonCredentialsRejected, c.Reason)
	require.Nil(t, st.Nodes)
}
