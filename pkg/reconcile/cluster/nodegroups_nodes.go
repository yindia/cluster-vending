package cluster

import (
	"context"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/clustersummary"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// nodeCounts are the Karpenter nodes by node pool.
type nodeCounts struct {
	count map[string]int32
	// rows are the nodes by node group (the default managed group is
	// "system"), collected during the same reads; complete: the node lists
	// were read to the end. Written to the results bucket, never kept in the
	// Cluster.
	rows     map[string][]*cloudv1.NodeGroupNode
	complete bool
	// usage sums the nodes (and, once read, the pods) per group for the
	// cluster status summary; nil when the summary is not due.
	usage *clustersummary.Aggregator
	// err is why the node lists could not be read, nil when they were.
	err error
}

// eachPage lists kind with limit nodeListLimit and continue, calling f with
// each page's items, until every page was read. Only a page is held at a
// time. ctx is the pass deadline: when it ends, the read stops with its
// error and what f saw is partial. A kind the cluster does not have
// (Karpenter's CRDs are not there yet) is a no-match error.
func eachPage(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, f func([]unstructured.Unstructured), opts ...client.ListOption) error {
	token := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page := &unstructured.UnstructuredList{}
		page.SetGroupVersionKind(gvk)
		pageOpts := append(slices.Clone(opts), client.Limit(nodeListLimit), client.Continue(token))
		if err := call(ctx, func(ctx context.Context) error { return c.List(ctx, page, pageOpts...) }); err != nil {
			return err
		}
		f(page.Items)
		if token = page.GetContinue(); token == "" {
			return nil
		}
	}
}

// readNodes counts the Karpenter nodes per pool and reads their kubelet
// minors, with paginated lists (limit 500 and continue, however many nodes
// there are, bounded by the pass deadline); only counts are kept for the
// status. The same reads collect the per-node rows (nodeCounts.rows): the
// Karpenter nodes by pool, any managed group's nodes as "system", each
// joined with its NodeClaim. A failed or unfinished read is "not known", and
// its rows are incomplete.
func (s *Reconciler) readNodes(ctx context.Context, c client.Client) (nodeCounts, kubeletView) {
	return s.readNodesInto(ctx, c, "", nil)
}

// readNodesInto is readNodes that also sums the nodes into usage when it is
// not nil.
func (s *Reconciler) readNodesInto(ctx context.Context, c client.Client, systemGroup string, usage *clustersummary.Aggregator) (nodeCounts, kubeletView) {
	counts := nodeCounts{count: map[string]int32{}, rows: map[string][]*cloudv1.NodeGroupNode{}, usage: usage}
	view := kubeletView{}
	minors := map[string]bool{}
	tenantMinors := map[string]map[string]bool{}
	byNode := map[string]*cloudv1.NodeGroupNode{}
	nodeList := schema.GroupVersionKind{Version: "v1", Kind: "NodeList"}
	err := eachPage(ctx, c, nodeList, func(items []unstructured.Unstructured) {
		for i := range items {
			n := &items[i]
			pool := n.GetLabels()[crossplaneaws.KarpenterNodePoolLabel]
			counts.count[pool]++
			row := clustersummary.NodeRow(n)
			counts.rows[pool] = append(counts.rows[pool], row)
			byNode[n.GetName()] = row
			if usage != nil {
				usage.AddNodeObject(pool, n)
			}
			if v, _, _ := unstructured.NestedString(n.Object, "status", "nodeInfo", "kubeletVersion"); v != "" {
				minor := kubernetesMinor(strings.TrimPrefix(v, "v"))
				switch {
				case platformPoolName(pool):
					minors[minor] = true
				case tenantMinors[pool] == nil:
					tenantMinors[pool] = map[string]bool{minor: true}
				default:
					tenantMinors[pool][minor] = true
				}
			}
		}
	}, client.HasLabels{crossplaneaws.KarpenterNodePoolLabel})
	if err != nil {
		s.logger().WarnContext(ctx, "node groups: list nodes", "error", err)
		return nodeCounts{count: map[string]int32{}, rows: counts.rows, usage: usage, err: err}, kubeletView{}
	}
	counts.complete = true
	// The default managed node group's nodes are the "system" group: the
	// nodes labelled with its name (any managed group's, where it is not
	// known).
	var managed client.ListOption = client.HasLabels{managedNodeGroupLabel}
	if systemGroup != "" {
		managed = client.MatchingLabels{managedNodeGroupLabel: systemGroup}
	}
	err = eachPage(ctx, c, nodeList, func(items []unstructured.Unstructured) {
		for i := range items {
			counts.count[systemNodeGroup]++
			counts.rows[systemNodeGroup] = append(counts.rows[systemNodeGroup], clustersummary.NodeRow(&items[i]))
			if usage != nil {
				usage.AddNodeObject(systemNodeGroup, &items[i])
			}
		}
	}, managed)
	if err != nil {
		s.logger().WarnContext(ctx, "node groups: list managed nodes", "error", err)
		counts.complete = false
		counts.err = err
	}
	for m := range minors {
		view.minors = append(view.minors, m)
	}
	for pool, ms := range tenantMinors {
		if view.tenant == nil {
			view.tenant = map[string][]string{}
		}
		for m := range ms {
			view.tenant[pool] = append(view.tenant[pool], m)
		}
	}
	err = eachPage(ctx, c, crossplaneaws.KarpenterNodeClaimGVK.GroupVersion().WithKind("NodeClaimList"), func(items []unstructured.Unstructured) {
		for i := range items {
			name, _, _ := unstructured.NestedString(items[i].Object, "status", "nodeName")
			// A claim of a tenant pool launching does not hold the step;
			// one without the pool label is counted.
			pool, labelled := items[i].GetLabels()[crossplaneaws.KarpenterNodePoolLabel]
			if name == "" && (!labelled || platformPoolName(pool)) {
				view.launching++
			} else if row := byNode[name]; row != nil {
				clustersummary.ClaimInto(row, &items[i])
			}
		}
	})
	switch {
	case err == nil:
		view.known = true
	case meta.IsNoMatchError(err):
		view.known = true
	default:
		s.logger().WarnContext(ctx, "node groups: list node claims", "error", err)
		counts.complete = false
	}
	return counts, view
}
