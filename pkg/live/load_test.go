package live

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// synthetic is a workload cluster of n nodes and pods pods whose list pages
// are made on request, never held: a fleet of them fits in a test. latency
// is each call's delay, inflight counts the calls in progress across the
// fleet.
type synthetic struct {
	nodes, pods int
	latency     time.Duration
	inflight    *atomic.Int32
	peak        *atomic.Int32
}

func (s synthetic) List(ctx context.Context, gvr schema.GroupVersionResource, limit int64, cont string) (*unstructured.UnstructuredList, error) {
	n := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for p := s.peak.Load(); n > p && !s.peak.CompareAndSwap(p, n); p = s.peak.Load() {
	}
	select {
	case <-time.After(s.latency):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	total := 0
	switch gvr {
	case NodesGVR:
		total = s.nodes
	case PodsGVR:
		total = s.pods
	}
	start, _ := strconv.Atoi(cont)
	end := min(start+int(limit), total)
	out := &unstructured.UnstructuredList{}
	for i := start; i < end; i++ {
		var o map[string]any
		if gvr == NodesGVR {
			o = node(fmt.Sprintf("n-%05d", i), "web", i%10 != 0, "4", "16Gi", t0.Add(-time.Duration(i)*time.Second))
		} else {
			o = pod(fmt.Sprintf("p-%06d", i), fmt.Sprintf("n-%05d", i%max(s.nodes, 1)), "Running", nil, ctr{"100m", "128Mi"})
		}
		out.Items = append(out.Items, unstructured.Unstructured{Object: o})
	}
	if end < total {
		out.SetContinue(strconv.Itoa(end))
	}
	return out, nil
}

type syntheticOpener map[string]synthetic

func (o syntheticOpener) Open(_ context.Context, tenantID, clusterID string) (Lister, error) {
	s, ok := o[tenantID+"/"+clusterID]
	if !ok {
		return nil, newError(ReasonUnreachable, "no cluster")
	}
	return s, nil
}

// The fleet of PROP-02 6.3 (60 small, 30 medium, 10 large clusters), every
// one asked for at once: never more than FleetInFlight builds reach the
// clusters, the ones that wait too long are refused as BUSY and served when
// asked again, a large cluster's sorted page is served warm in well under
// 100 ms, and the views stay within their byte bound. In process, with a
// fixed 2 ms per call: it checks the limits, not the latency a real
// apiserver has (phase 3's load test on real clusters measures that).
func TestLoadSyntheticFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("the synthetic fleet lists about 180,000 pods")
	}
	var inflight, peak atomic.Int32
	fleet := syntheticOpener{}
	sizes := []struct{ count, nodes, pods int }{{60, 20, 300}, {30, 200, 2000}, {10, 1000, 10000}}
	tenants := 0
	for _, size := range sizes {
		for range size.count {
			// One cluster per tenant: the per-tenant rate is not what is measured.
			fleet[fmt.Sprintf("t%03d/c", tenants)] = synthetic{nodes: size.nodes, pods: size.pods, latency: 2 * time.Millisecond, inflight: &inflight, peak: &peak}
			tenants++
		}
	}
	lim := DefaultLimits()
	r := New(fleet, lim, slog.New(slog.DiscardHandler))

	// Cold: a 1,000-node, 10,000-pod cluster read and sorted on a first
	// request, alone.
	cold := New(fleet, lim, slog.New(slog.DiscardHandler))
	start := time.Now()
	if _, err := cold.ListNodeGroupNodes(t.Context(), &cloudv1.ListNodeGroupNodesRequest{TenantId: "t099", ClusterId: "c", NodeGroup: "web",
		SortBy: cloudv1.NodeSortBy_NODE_SORT_BY_MEMORY_REQUESTED_PERCENT}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("cold page of a large cluster took %s, want under 3 s", took)
	} else {
		t.Logf("cold sorted page of a 1,000-node, 10,000-pod cluster: %s (in process, 2 ms a call)", took)
	}
	peak.Store(0)

	served := make([]bool, tenants)
	var mu sync.Mutex
	var busy atomic.Int32
	for round := 0; round < 20 && slices.Contains(served, false); round++ {
		var wg sync.WaitGroup
		for i := range tenants {
			if served[i] {
				continue
			}
			wg.Go(func() {
				_, err := r.ListNodeGroupNodes(t.Context(), &cloudv1.ListNodeGroupNodesRequest{TenantId: fmt.Sprintf("t%03d", i), ClusterId: "c", NodeGroup: "web",
					SortBy: cloudv1.NodeSortBy_NODE_SORT_BY_CPU_REQUESTED_PERCENT, SortOrder: cloudv1.SortOrder_SORT_ORDER_DESC})
				switch reasonOf(err) {
				case "":
					if err != nil {
						t.Errorf("t%03d: %v", i, err)
						return
					}
					mu.Lock()
					served[i] = true
					mu.Unlock()
				case ReasonBusy:
					busy.Add(1)
				default:
					t.Errorf("t%03d: %v", i, err)
				}
			})
		}
		wg.Wait()
	}
	if slices.Contains(served, false) {
		t.Fatalf("not every cluster was served: %v", served)
	}
	if p := peak.Load(); p > int32(lim.FleetInFlight) {
		t.Fatalf("%d calls at once across the fleet, want at most %d (one per build in flight)", p, lim.FleetInFlight)
	}
	t.Logf("fleet: %d clusters served, %d BUSY refusals, peak %d builds reading at once", tenants, busy.Load(), peak.Load())

	// Warm: the large cluster's build is reused.
	start = time.Now()
	res, err := r.ListNodeGroupNodes(t.Context(), &cloudv1.ListNodeGroupNodesRequest{TenantId: "t099", ClusterId: "c", NodeGroup: "web",
		SortBy: cloudv1.NodeSortBy_NODE_SORT_BY_POD_COUNT})
	warm := time.Since(start)
	if err != nil || res.GetTotal() != 1000 || len(res.GetNodes()) != lim.DefaultPageSize {
		t.Fatalf("warm page = %v, %v", res.GetTotal(), err)
	}
	if warm > 100*time.Millisecond {
		t.Fatalf("warm page took %s, want under 100 ms", warm)
	}
	if size := r.store.size(); size > lim.ViewBytes {
		t.Fatalf("views hold %d bytes, over the bound %d", size, lim.ViewBytes)
	}
	t.Logf("warm sorted page of a 1,000-node cluster: %s; views: %d KiB", warm, r.store.size()>>10)
}
