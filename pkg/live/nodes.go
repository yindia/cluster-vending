package live

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/yindia/cluster-vending/pkg/clustersummary"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The labels that put a node in a node group: Karpenter's NodePool (the
// platform pool and the user groups), and the managed node group's, whose
// nodes are the "system" group.
const (
	labelNodePool    = "karpenter.sh/nodepool"
	labelManagedPool = "eks.amazonaws.com/nodegroup"
	// SystemGroup is the group of the default managed node group's nodes.
	SystemGroup = "system"
)

// Notices of a nodes build.
const (
	NoticeNoUsage = "node usage unavailable: metrics.k8s.io is not served"
)

// rowOverhead is what a kept row costs beyond its encoded size (pointers,
// the group's name): an estimate for the views' byte bound.
const rowOverhead = 96

// nodeRow is one node of a build and the group it belongs to.
type nodeRow struct {
	group string
	node  *cloudv1.NodeGroupNode
}

// nodeBuild is one read of a cluster's nodes, pods, NodeClaims and node
// metrics: a row per node with its requests and usage, and the totals per
// group and for the cluster. Pods are projected as they arrive and never
// kept. Immutable once built.
type nodeBuild struct {
	liveAt  time.Time
	rows    []nodeRow
	groups  []*cloudv1.NodeResourceTotals
	cluster *cloudv1.NodeResourceTotals
	notices []string
	bytes   int64
	pages   int
}

// groupOf is the node group of a node by its labels; "" for a node of no
// group the platform knows.
func groupOf(labels map[string]string) string {
	if g := labels[labelNodePool]; g != "" {
		return g
	}
	if labels[labelManagedPool] != "" {
		return SystemGroup
	}
	return ""
}

// buildNodes reads the nodes view. A continue token that expires (410)
// restarts the build once; a second expiry fails it as CLUSTER_UNREACHABLE
// with the reason "list expired".
func buildNodes(ctx context.Context, l Lister, lim Limits, now time.Time) (*nodeBuild, error) {
	b, err := buildNodesOnce(ctx, l, lim, now)
	if errors.Is(err, errExpired) {
		b, err = buildNodesOnce(ctx, l, lim, now)
		if errors.Is(err, errExpired) {
			return nil, newError(ReasonUnreachable, "list expired")
		}
	}
	return b, err
}

func buildNodesOnce(ctx context.Context, l Lister, lim Limits, now time.Time) (*nodeBuild, error) {
	pages := &budget{max: lim.MaxPages}
	byName := map[string]*nodeRow{}
	var rows []nodeRow
	ungrouped := 0
	over := false
	err := eachPage(ctx, l, NodesGVR, lim.PageLimit, pages, func(u *unstructured.Unstructured) {
		group := groupOf(u.GetLabels())
		if group == "" {
			ungrouped++
			return
		}
		if len(rows) >= lim.MaxNodeRows {
			over = true
			return
		}
		rows = append(rows, nodeRow{group: group, node: nodeOf(u)})
	})
	if errors.Is(err, errNotServed) {
		err = newError(ReasonUnreachable, "the workload cluster does not serve nodes")
	}
	if err != nil {
		return nil, err
	}
	if over {
		return nil, newError(ReasonLimitExceeded, "the cluster has more than %d nodes", lim.MaxNodeRows)
	}
	for i := range rows {
		byName[rows[i].node.GetName()] = &rows[i]
	}
	orphans := 0
	err = eachPage(ctx, l, PodsGVR, lim.PageLimit, pages, func(u *unstructured.Unstructured) {
		var p corev1.Pod
		if runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p) != nil || !clustersummary.Counted(&p) {
			return
		}
		r, ok := byName[p.Spec.NodeName]
		if !ok {
			orphans++
			return
		}
		req := clustersummary.PodRequests(&p)
		r.node.RequestedCpuMillis += req.CPUMillis
		r.node.RequestedMemoryBytes += req.MemoryBytes
		r.node.PodCount++
		if req.MissingRequests {
			r.node.PodsWithoutRequests++
		}
	})
	if errors.Is(err, errNotServed) {
		err = newError(ReasonUnreachable, "the workload cluster does not serve pods")
	}
	if err != nil {
		return nil, err
	}
	err = eachPage(ctx, l, NodeClaimsGVR, lim.PageLimit, pages, func(u *unstructured.Unstructured) {
		name, _, _ := unstructured.NestedString(u.Object, "status", "nodeName")
		if r, ok := byName[name]; ok && name != "" {
			clustersummary.ClaimInto(r.node, u)
		}
	})
	if err != nil && !errors.Is(err, errNotServed) {
		return nil, err
	}
	out := &nodeBuild{liveAt: now, rows: rows}
	usage := readUsage(ctx, l, lim, pages, byName)
	if !usage {
		out.notices = append(out.notices, NoticeNoUsage)
	}
	if orphans > 0 {
		out.notices = append(out.notices, fmt.Sprintf("%d pods are bound to nodes not in the read (the node list and the pod list are separate reads)", orphans))
	}
	if ungrouped > 0 {
		out.notices = append(out.notices, fmt.Sprintf("%d nodes belong to no node group of the platform and are left out", ungrouped))
	}
	out.groups, out.cluster = totals(rows, usage)
	for _, r := range rows {
		out.bytes += int64(proto.Size(r.node)) + rowOverhead
	}
	out.pages = pages.pages
	return out, nil
}

// readUsage puts each node's usage from metrics.k8s.io on its row; false when
// the metrics cannot be read (not served, or the list failed): the rows then
// carry no usage. A cap hit here leaves the usage out rather than failing the
// build.
func readUsage(ctx context.Context, l Lister, lim Limits, pages *budget, byName map[string]*nodeRow) bool {
	type use struct{ cpu, mem int64 }
	read := map[string]use{}
	err := eachPage(ctx, l, NodeMetricsGVR, lim.PageLimit, pages, func(u *unstructured.Unstructured) {
		if _, ok := byName[u.GetName()]; !ok {
			return
		}
		cpu, _, _ := unstructured.NestedString(u.Object, "usage", "cpu")
		mem, _, _ := unstructured.NestedString(u.Object, "usage", "memory")
		read[u.GetName()] = use{cpu: milliOf(cpu), mem: valueOf(mem)}
	})
	if err != nil {
		return false
	}
	for name, r := range byName {
		if v, ok := read[name]; ok {
			r.node.UsedCpuMillis, r.node.UsedMemoryBytes = proto.Int64(v.cpu), proto.Int64(v.mem)
		}
	}
	return true
}

// nodeOf is the row of a node (clustersummary.NodeRow, the controller's
// projection) with its allocatable resources.
func nodeOf(u *unstructured.Unstructured) *cloudv1.NodeGroupNode {
	row := clustersummary.NodeRow(u)
	alloc, _, _ := unstructured.NestedStringMap(u.Object, "status", "allocatable")
	row.AllocatableCpuMillis = milliOf(alloc["cpu"])
	row.AllocatableMemoryBytes = valueOf(alloc["memory"])
	row.AllocatablePods = int32(min(valueOf(alloc["pods"]), 1<<30)) //nolint:gosec // bounded above
	return row
}

func milliOf(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.MilliValue()
}

func valueOf(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.Value()
}

// totals are the sums per group (in name order) and for the cluster. The
// used sums are set only when the usage was read.
func totals(rows []nodeRow, usage bool) ([]*cloudv1.NodeResourceTotals, *cloudv1.NodeResourceTotals) {
	by := map[string]*cloudv1.NodeResourceTotals{}
	cluster := &cloudv1.NodeResourceTotals{}
	for _, r := range rows {
		g := by[r.group]
		if g == nil {
			g = &cloudv1.NodeResourceTotals{NodeGroup: r.group}
			by[r.group] = g
		}
		add(g, r.node, usage)
		add(cluster, r.node, usage)
	}
	groups := make([]*cloudv1.NodeResourceTotals, 0, len(by))
	for _, g := range by {
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b *cloudv1.NodeResourceTotals) int { return strings.Compare(a.GetNodeGroup(), b.GetNodeGroup()) })
	if usage && cluster.UsedCpuMillis == nil {
		cluster.UsedCpuMillis, cluster.UsedMemoryBytes = proto.Int64(0), proto.Int64(0)
	}
	return groups, cluster
}

func add(t *cloudv1.NodeResourceTotals, n *cloudv1.NodeGroupNode, usage bool) {
	t.Nodes++
	if n.GetReady() {
		t.ReadyNodes++
	}
	t.AllocatableCpuMillis += n.GetAllocatableCpuMillis()
	t.AllocatableMemoryBytes += n.GetAllocatableMemoryBytes()
	t.AllocatablePods += int64(n.GetAllocatablePods())
	t.RequestedCpuMillis += n.GetRequestedCpuMillis()
	t.RequestedMemoryBytes += n.GetRequestedMemoryBytes()
	t.PodCount += int64(n.GetPodCount())
	t.PodsWithoutRequests += int64(n.GetPodsWithoutRequests())
	if usage {
		t.UsedCpuMillis = proto.Int64(t.GetUsedCpuMillis() + n.GetUsedCpuMillis())
		t.UsedMemoryBytes = proto.Int64(t.GetUsedMemoryBytes() + n.GetUsedMemoryBytes())
	}
}
