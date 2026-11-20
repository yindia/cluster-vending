package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/stretchr/testify/require"
	awss3 "github.com/upbound/provider-aws/v2/apis/namespaced/s3/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/results/resultstest"
)

var rowsAt = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// rowsWorld is a node group world whose controller can write results: a
// Bucket managed resource and an in-memory S3.
type rowsWorld struct {
	world
	row    *model.Cluster
	s3     *resultstest.S3
	bucket string
	fw     *fakeWorkload
}

func newRowsWorld(t *testing.T, fw *fakeWorkload, bucketReady bool, groups ...*cloudv1.NodeGroupSpec) *rowsWorld {
	t.Helper()
	rw := &rowsWorld{fw: fw}
	w, src := platformWorld(t, func(d *reconcile.Deps) {
		d.NodeGroupClients = fw
		d.ResultsS3 = func(awscloud.Access, string) (results.S3API, error) { return rw.s3, nil }
	})
	src.release.Spec.Content.Kubernetes[0].NodeAMIPin = "v20261107"
	row := w.cluster(t)
	spec, err := SpecOf(row)
	require.NoError(t, err)
	spec.NodeGroups = groups
	raw, err := Encode(spec)
	require.NoError(t, err)
	row.Spec = raw
	require.NoError(t, w.store.Clusters().Update(t.Context(), row))
	rw.world, rw.row = w, w.cluster(t)
	iamFake{role: true, access: true, policy: true, attachments: 4, passRoleOn: nodeRoleARN(rw.row)}.seed(t, w, rw.row, "us-east-1")

	prefix := crossplaneaws.ClusterPrefix(worldTenant, rw.row.ID, rw.row.Name)
	rw.bucket = crossplaneaws.ResultsBucketName(prefix, "444455556666", rw.row.Region, rw.row.ID)
	rw.s3 = resultstest.New(rw.bucket)
	b := &awss3.Bucket{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: crossplaneaws.ResultsBucketObjectName(prefix)}}
	if bucketReady {
		b.SetConditions(xpv2.Available())
	} else {
		b.SetConditions(xpv2.Creating())
	}
	require.NoError(t, w.kube.Create(t.Context(), b))

	was := now
	now = func() time.Time { return rowsAt }
	t.Cleanup(func() { now = was })
	return rw
}

// pass runs one node group pass on the cluster as it is stored now.
func (rw *rowsWorld) pass(t *testing.T) *nodeGroupsResult {
	t.Helper()
	return passOf(rw.world, rw.cluster(t), karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
}

func (rw *rowsWorld) at(d time.Duration) { now = func() time.Time { return rowsAt.Add(d) } }

func (rw *rowsWorld) shard(t *testing.T, group string) *cloudv1.NodeGroupNodes {
	t.Helper()
	into := &cloudv1.NodeGroupNodes{}
	_, _, err := results.NewReader(rw.s3).LatestShard(t.Context(), rw.bucket, results.Nodes, group, into)
	require.NoError(t, err, group)
	return into
}

func (rw *rowsWorld) index(t *testing.T) *cloudv1.ResultIndex {
	t.Helper()
	idx, _, _, err := results.NewReader(rw.s3).Index(t.Context(), rw.bucket, results.Nodes)
	require.NoError(t, err)
	return idx
}

func (rw *rowsWorld) puts() int { return len(rw.s3.Puts) }

func karpenterNode(name, pool string, ready bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(rowsAt.Add(-time.Hour)), Labels: map[string]string{
		crossplaneaws.KarpenterNodePoolLabel: pool, "node.kubernetes.io/instance-type": "m7i.large", "topology.kubernetes.io/zone": "us-east-1a",
		"karpenter.sh/capacity-type": "spot", "kubernetes.io/arch": "amd64"}},
		Spec:   corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-" + name},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.37.1-eks"}}}
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	return n
}

func managedNode(name string) *corev1.Node {
	n := karpenterNode(name, "", true)
	delete(n.Labels, crossplaneaws.KarpenterNodePoolLabel)
	delete(n.Labels, "karpenter.sh/capacity-type")
	n.Labels["eks.amazonaws.com/nodegroup"] = "prod-default"
	n.Labels["eks.amazonaws.com/capacityType"] = "ON_DEMAND"
	return n
}

func nodeClaim(name, node string, conds ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.KarpenterNodeClaimGVK)
	u.SetName(name)
	var cs []any
	for _, c := range conds {
		cs = append(cs, map[string]any{"type": c, "status": "True"})
	}
	u.Object["status"] = map[string]any{"nodeName": node, "providerID": "aws:///x/i-" + node, "conditions": cs}
	return u
}

func TestNodeRowsAreCollectedPerGroupAndWritten(t *testing.T) {
	gpu1, gpu2, gpu3 := karpenterNode("ip-10-0-1-1", "gpu", true), karpenterNode("ip-10-0-1-2", "gpu", false), karpenterNode("ip-10-0-1-3", "gpu", true)
	gpu2.Spec.Unschedulable = true
	gpu2.Spec.Taints = []corev1.Taint{{Key: "a"}, {Key: "b"}}
	gpu2.Status.Conditions = append(gpu2.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
		corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse}, corev1.NodeCondition{Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionTrue})
	gpu3.Labels["kubernetes.io/arch"] = "arm64"
	fw := newFakeWorkload(interceptor.Funcs{}, gpu1, gpu2, gpu3, karpenterNode("ip-10-0-2-1", "platform", true), managedNode("ip-10-0-3-1"), managedNode("ip-10-0-3-2"),
		nodeClaim("gpu-abc", "ip-10-0-1-1", "Launched", "Registered", "Initialized", "Ready"), nodeClaim("gpu-def", "ip-10-0-1-2", "Launched", "Registered"))
	rw := newRowsWorld(t, fw, true, &cloudv1.NodeGroupSpec{Name: "gpu", InstanceFamilies: []string{"g5"}})

	res := rw.pass(t)

	// Counts stay in the CR; no per-node rows.
	require.Equal(t, int32(3), statusOf(res, "gpu").GetNodes(), "the count stays in the status")
	for _, g := range res.groups {
		require.Empty(t, g.GetNodesNotice())
	}
	idx := rw.index(t)
	require.True(t, idx.GetComplete())
	var names []string
	for _, s := range idx.GetShards() {
		names = append(names, s.GetShard())
	}
	require.Equal(t, []string{"gpu", "platform", "system"}, names)

	gpu := rw.shard(t, "gpu")
	require.Equal(t, "gpu", gpu.GetGroup())
	require.Len(t, gpu.GetNodes(), 3)
	n1, n2, n3 := gpu.GetNodes()[0], gpu.GetNodes()[1], gpu.GetNodes()[2]
	require.Equal(t, "ip-10-0-1-1", n1.GetName(), "sorted by name")
	require.Equal(t, "m7i.large", n1.GetInstanceType())
	require.Equal(t, "us-east-1a", n1.GetZone())
	require.Equal(t, "spot", n1.GetCapacityType())
	require.Equal(t, "amd64", n1.GetArchitecture())
	require.Equal(t, "v1.37.1-eks", n1.GetKubeletVersion())
	require.True(t, n1.GetReady())
	require.Equal(t, "aws:///us-east-1a/i-ip-10-0-1-1", n1.GetProviderId())
	require.True(t, n1.GetCreatedAt().AsTime().Equal(rowsAt.Add(-time.Hour)))
	require.Equal(t, "gpu-abc", n1.GetNodeclaimName())
	require.Equal(t, "Initialized", n1.GetNodeclaimPhase())
	require.True(t, n1.GetNodeclaimReady())
	require.False(t, n2.GetReady())
	require.True(t, n2.GetUnschedulable())
	require.EqualValues(t, 2, n2.GetTaintsCount())
	require.Equal(t, []string{"MemoryPressure", "NetworkUnavailable"}, n2.GetConditions(), "only the conditions that are True")
	require.Equal(t, "gpu-def", n2.GetNodeclaimName())
	require.Equal(t, "Registered", n2.GetNodeclaimPhase())
	require.False(t, n2.GetNodeclaimReady())
	require.Equal(t, "arm64", n3.GetArchitecture())
	require.Empty(t, n3.GetNodeclaimName(), "a node without a claim")

	system := rw.shard(t, "system")
	require.Len(t, system.GetNodes(), 2)
	require.Equal(t, "on-demand", system.GetNodes()[0].GetCapacityType())
	require.Empty(t, system.GetNodes()[0].GetNodeclaimName())

	// The summary on the Cluster names the index and the total.
	ref := rw.cluster(t).ResultRef("nodes")
	require.NotNil(t, ref)
	require.Equal(t, results.Nodes.IndexKey(), ref.Key)
	require.EqualValues(t, 6, ref.Total)
	require.True(t, ref.CollectedAt.Equal(rowsAt))
	// The platform and the managed group's nodes count without being rows of the CR.
	require.Equal(t, int32(2), statusOf(res, "system").GetNodes())
}

func TestNodeRowsAreWrittenAtMostOncePerMinuteAndOnlyWhenChanged(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{}, karpenterNode("a", "platform", true), karpenterNode("b", "gpu", true))
	rw := newRowsWorld(t, fw, true, &cloudv1.NodeGroupSpec{Name: "gpu", InstanceFamilies: []string{"g5"}})
	rw.pass(t)
	first := rw.puts()
	require.Positive(t, first)

	// A node appears at once, inside the minute: not written yet.
	require.NoError(t, fw.c.Create(t.Context(), karpenterNode("c", "gpu", true)))
	rw.at(59 * time.Second)
	rw.pass(t)
	require.Equal(t, first, rw.puts(), "throttled: at most once a minute per cluster")
	require.Len(t, rw.shard(t, "gpu").GetNodes(), 1)

	// After the minute the change is written: its shard and the index only.
	rw.at(61 * time.Second)
	before := rw.puts()
	rw.pass(t)
	require.Equal(t, before+3, rw.puts(), "the changed shard (history and latest) and the index")
	require.Len(t, rw.shard(t, "gpu").GetNodes(), 2)
	require.Len(t, rw.shard(t, "platform").GetNodes(), 1)

	// After another minute, unchanged: nothing is written.
	rw.at(3 * time.Minute)
	before = rw.puts()
	rw.pass(t)
	require.Equal(t, before, rw.puts(), "the same digest writes nothing")

	// A removed group's shard goes.
	require.NoError(t, fw.c.Delete(t.Context(), karpenterNode("a", "platform", true)))
	rw.at(5 * time.Minute)
	rw.pass(t)
	_, _, ok := rw.s3.Object(rw.bucket, results.Nodes.ShardLatestKey("platform"))
	require.False(t, ok)
	require.Len(t, rw.index(t).GetShards(), 1)
}

// An unchanged collection moves the summary's time now and then, so the
// console's "collected" does not age for a quiet cluster.
func TestUnchangedNodeRowsRefreshTheSummaryTimeSlowly(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{}, karpenterNode("a", "platform", true))
	rw := newRowsWorld(t, fw, true)
	rw.pass(t)
	collected := func() time.Time { return rw.cluster(t).ResultRef("nodes").CollectedAt }
	require.True(t, collected().Equal(rowsAt))

	rw.at(2 * time.Minute)
	rw.pass(t)
	require.True(t, collected().Equal(rowsAt), "not for every pass")

	rw.at(11 * time.Minute)
	rw.pass(t)
	require.True(t, collected().Equal(rowsAt.Add(11*time.Minute)))
}

func TestNodeRowsNeedingNoS3AreSilent(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{}, karpenterNode("a", "platform", true))
	w, _ := nodeGroupWorld(t, fw) // no ResultsS3: a test, or a controller with no AWS
	res := passOf(w, w.cluster(t), karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	require.Equal(t, int32(1), statusOf(res, "platform").GetNodes())
	for _, g := range res.groups {
		require.Empty(t, g.GetNodesNotice())
	}
}

// When S3 cannot be written the pass still succeeds: counts are in the CR,
// there are no rows, and the group's status says why.
func TestNodeRowsWhenS3IsUnavailable(t *testing.T) {
	for name, tt := range map[string]struct {
		ready  bool
		fail   func(op, bucket, key string) error
		notice string
	}{
		"bucket not ready": {false, nil, "the results bucket is not ready yet"},
		"write failed": {true, func(op, _, _ string) error {
			if op == "PutObject" {
				return errors.New("boom")
			}
			return nil
		}, "writing to the results bucket failed"},
	} {
		t.Run(name, func(t *testing.T) {
			fw := newFakeWorkload(interceptor.Funcs{}, karpenterNode("a", "platform", true), karpenterNode("b", "gpu", true))
			rw := newRowsWorld(t, fw, tt.ready, &cloudv1.NodeGroupSpec{Name: "gpu", InstanceFamilies: []string{"g5"}})
			rw.s3.Fail = tt.fail

			res := rw.pass(t)

			require.Equal(t, int32(1), statusOf(res, "gpu").GetNodes(), "counts are still there")
			for _, g := range res.groups {
				require.Equal(t, "the node list is unavailable: "+tt.notice, g.GetNodesNotice(), g.GetName())
			}
			require.Nil(t, rw.cluster(t).ResultRef("nodes"))
			require.Empty(t, rw.s3.Keys(rw.bucket))
		})
	}
}

// A throttled pass keeps the notice of the last attempt.
func TestThrottledPassKeepsTheNodesNotice(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{}, karpenterNode("a", "platform", true))
	rw := newRowsWorld(t, fw, false)
	first := rw.pass(t)
	require.NotEmpty(t, statusOf(first, "platform").GetNodesNotice())
	// The recorded status is what the next pass starts from.
	st := &cloudv1.ClusterStatus{NodeGroups: first.groups}
	rw.rememberStatus(t, st)
	rw.at(10 * time.Second)
	second := rw.pass(t)
	require.Equal(t, statusOf(first, "platform").GetNodesNotice(), statusOf(second, "platform").GetNodesNotice())
}

// Reading is paginated, so 1,200 nodes of one group are 1,200 rows of one
// shard.
func TestNodeRowsOfAThousandNodesAreOneShard(t *testing.T) {
	calls := 0
	fw := newFakeWorkload(pagedNodes(1200, &calls, nil))
	rw := newRowsWorld(t, fw, true)

	rw.pass(t)

	require.Len(t, rw.shard(t, "platform").GetNodes(), 1200)
	require.EqualValues(t, 1200, rw.index(t).GetShards()[0].GetCount())
	require.EqualValues(t, 1200, rw.cluster(t).ResultRef("nodes").Total)
}

// A pass that runs out of time writes what it read, marked incomplete.
func TestNodeRowsOfAnUnfinishedReadAreIncomplete(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fw := newFakeWorkload(pagedNodes(1200, &calls, func(page int) {
		if page == 2 {
			cancel()
		}
	}))
	rw := newRowsWorld(t, fw, true)
	// The pass's own deadline ends the read; S3 writes use a fresh context.
	nodes, _ := rw.svc.readNodes(ctx, fw.c)
	require.False(t, nodes.complete)
	require.NotEmpty(t, nodes.rows["platform"])
	require.Less(t, len(nodes.rows["platform"]), 1200)
}

func TestNodeRowsSkipAGroupThatCannotBeAShard(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{}, karpenterNode("a", "index", true), karpenterNode("b", "platform", true), karpenterNode("c", "Weird_Pool", true))
	rw := newRowsWorld(t, fw, true)
	rw.pass(t)
	require.Len(t, rw.index(t).GetShards(), 1)
	require.Equal(t, "platform", rw.index(t).GetShards()[0].GetShard())
}

func (rw *rowsWorld) rememberStatus(t *testing.T, st *cloudv1.ClusterStatus) {
	t.Helper()
	raw, err := Encode(st)
	require.NoError(t, err)
	row := rw.cluster(t)
	require.NoError(t, rw.world.store.Clusters().SetStatus(t.Context(), worldTenant, row.ID, row.ETag, raw))
}
