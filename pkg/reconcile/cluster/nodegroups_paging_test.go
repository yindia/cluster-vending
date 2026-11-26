package cluster

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// pagedNodes serves n Karpenter nodes of the "platform" pool in pages of the
// limit asked for, with a continue token, as an API server does. It counts
// the List calls.
func pagedNodes(n int, calls *int, onPage func(page int)) interceptor.Funcs {
	return interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		u, ok := list.(*unstructured.UnstructuredList)
		if !ok || u.GetKind() != "NodeList" {
			return nil // no NodeClaims: an empty list
		}
		lo := &client.ListOptions{}
		lo.ApplyOptions(opts)
		if lo.Limit == 0 {
			return fmt.Errorf("unbounded list")
		}
		start := 0
		if lo.Continue != "" {
			start, _ = strconv.Atoi(lo.Continue)
		}
		pool := labels.Set{crossplaneaws.KarpenterNodePoolLabel: "platform"}
		if lo.LabelSelector != nil && !lo.LabelSelector.Matches(pool) {
			return nil // the selector (the managed group's label) matches none of these nodes
		}
		*calls++
		if onPage != nil {
			onPage(*calls)
		}
		end := min(start+int(lo.Limit), n)
		for i := start; i < end; i++ {
			node := unstructured.Unstructured{}
			node.SetAPIVersion("v1")
			node.SetKind("Node")
			node.SetName(fmt.Sprintf("node-%d", i))
			node.SetLabels(pool)
			_ = unstructured.SetNestedField(node.Object, "v1.36.1-eks", "status", "nodeInfo", "kubeletVersion")
			u.Items = append(u.Items, node)
		}
		if end < n {
			u.SetContinue(strconv.Itoa(end))
		}
		return nil
	}}
}

// 1,200 nodes are counted in full: three pages of at most 500, no cap.
func TestReadNodesCountsEveryNodeAcrossPages(t *testing.T) {
	calls := 0
	w, _ := nodeGroupWorld(t, newFakeWorkload(interceptor.Funcs{}))
	c := newFakeWorkload(pagedNodes(1200, &calls, nil)).c

	counts, view := w.svc.readNodes(t.Context(), c)

	require.Equal(t, int32(1200), counts.count["platform"])
	require.True(t, view.known, "every page was read")
	require.Equal(t, []string{"1.36"}, view.minors)
	require.Equal(t, 3, calls)
}

// A pass that runs out of time says "not known" and counts nothing it half
// read.
func TestReadNodesStopsAtThePassDeadline(t *testing.T) {
	calls := 0
	w, _ := nodeGroupWorld(t, newFakeWorkload(interceptor.Funcs{}))
	ctx, cancel := context.WithCancel(t.Context())
	c := newFakeWorkload(pagedNodes(1200, &calls, func(page int) {
		if page == 2 {
			cancel()
		}
	})).c

	_, view := w.svc.readNodes(ctx, c)

	require.False(t, view.known)
	require.LessOrEqual(t, calls, 3)
}
