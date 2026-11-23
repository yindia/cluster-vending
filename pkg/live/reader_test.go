package live

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const tenant, clusterID = "acme", "01a1d2f6-12a9-7505-b7b2-8b8acfcab306"

var quiet = slog.New(slog.DiscardHandler)

func newReader(t *testing.T, lim Limits, clusters map[string]*fakeCluster) (*Reader, *fakeOpener, *clock) {
	t.Helper()
	o := &fakeOpener{clusters: clusters}
	r := New(o, lim, quiet)
	c := &clock{t: t0}
	r.now = c.now
	return r, o, c
}

// many is a cluster with n nodes in group "web".
func many(n int) *fakeCluster {
	f := newFake()
	for i := range n {
		f.add(NodesGVR, node(fmt.Sprintf("web-%03d", i), "web", i%3 != 0, "4", "16Gi", t0.Add(-time.Duration(i)*time.Minute)))
		f.add(PodsGVR, pod(fmt.Sprintf("p-%03d", i), fmt.Sprintf("web-%03d", i), "Running", nil, ctr{fmt.Sprintf("%dm", 100*(i%7)), "1Gi"}))
	}
	return f
}

func listReq(token string) *cloudv1.ListNodeGroupNodesRequest {
	return &cloudv1.ListNodeGroupNodesRequest{TenantId: tenant, ClusterId: clusterID, NodeGroup: "web", PageSize: 4, PageToken: token}
}

func reasonOf(err error) string {
	var le *Error
	if errors.As(err, &le) {
		return le.Reason
	}
	return ""
}

func TestListNodesPagesASortedView(t *testing.T) {
	f := many(10)
	r, _, _ := newReader(t, DefaultLimits(), map[string]*fakeCluster{tenant + "/" + clusterID: f})
	req := listReq("")
	req.SortBy, req.SortOrder = cloudv1.NodeSortBy_NODE_SORT_BY_CPU_REQUESTED_PERCENT, cloudv1.SortOrder_SORT_ORDER_DESC
	var names []string
	var calls int
	for page := 0; ; page++ {
		res, err := r.ListNodeGroupNodes(t.Context(), req)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if res.GetLiveAt() == nil || !res.GetComplete() || res.GetTotal() != 10 {
			t.Fatalf("page %d = %v", page, res)
		}
		for _, n := range res.GetNodes() {
			names = append(names, n.GetName())
		}
		if page == 0 {
			calls = f.callCount()
		}
		if res.GetNextPageToken() == "" {
			break
		}
		if !strings.HasPrefix(res.GetNextPageToken(), "live:") {
			t.Fatalf("token %q", res.GetNextPageToken())
		}
		req.PageToken = res.GetNextPageToken()
	}
	if len(names) != 10 {
		t.Fatalf("names = %v", names)
	}
	if f.callCount() != calls {
		t.Fatal("later pages are served from the view, not the cluster")
	}
	// 600m is the most requested (i%7 == 6: web-006); ties by name.
	if names[0] != "web-006" {
		t.Fatalf("order = %v", names)
	}
}

func TestListNodesTokens(t *testing.T) {
	tests := []struct {
		name   string
		change func(req *cloudv1.ListNodeGroupNodesRequest, r *Reader, c *clock) *Reader
	}{
		{name: "a sort change", change: func(req *cloudv1.ListNodeGroupNodesRequest, r *Reader, _ *clock) *Reader {
			req.SortBy = cloudv1.NodeSortBy_NODE_SORT_BY_AGE
			return r
		}},
		{name: "an order change", change: func(req *cloudv1.ListNodeGroupNodesRequest, r *Reader, _ *clock) *Reader {
			req.SortOrder = cloudv1.SortOrder_SORT_ORDER_DESC
			return r
		}},
		{name: "a filter change", change: func(req *cloudv1.ListNodeGroupNodesRequest, r *Reader, _ *clock) *Reader {
			req.Filter = cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_READY
			return r
		}},
		{name: "another tenant", change: func(req *cloudv1.ListNodeGroupNodesRequest, r *Reader, _ *clock) *Reader {
			req.TenantId = "other"
			return r
		}},
		{name: "another group", change: func(req *cloudv1.ListNodeGroupNodesRequest, r *Reader, _ *clock) *Reader {
			req.NodeGroup = "system"
			return r
		}},
		{name: "idle past 60 seconds", change: func(_ *cloudv1.ListNodeGroupNodesRequest, r *Reader, c *clock) *Reader {
			c.advance(61 * time.Second)
			return r
		}},
		{name: "built more than 5 minutes ago", change: func(req *cloudv1.ListNodeGroupNodesRequest, r *Reader, c *clock) *Reader {
			for range 7 { // used every 50 seconds: never idle, but old
				c.advance(50 * time.Second)
				if _, err := r.ListNodeGroupNodes(t.Context(), req); err != nil {
					break
				}
			}
			return r
		}},
		{name: "a token from before a restart", change: func(_ *cloudv1.ListNodeGroupNodesRequest, r *Reader, c *clock) *Reader {
			restarted := New(r.open, r.lim, quiet)
			restarted.now = c.now
			return restarted
		}},
		{name: "a stored list's token", change: func(req *cloudv1.ListNodeGroupNodesRequest, r *Reader, _ *clock) *Reader {
			req.PageToken = "4"
			return r
		}},
		{name: "a forged view ID", change: func(req *cloudv1.ListNodeGroupNodesRequest, r *Reader, _ *clock) *Reader {
			req.PageToken = viewToken(strings.Repeat("0", 32), 4)
			return r
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _, c := newReader(t, DefaultLimits(), map[string]*fakeCluster{tenant + "/" + clusterID: many(10)})
			first, err := r.ListNodeGroupNodes(t.Context(), listReq(""))
			if err != nil || first.GetNextPageToken() == "" {
				t.Fatalf("first page: %v %v", first, err)
			}
			req := listReq(first.GetNextPageToken())
			r = tt.change(req, r, c)
			_, err = r.ListNodeGroupNodes(t.Context(), req)
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), MessageRestart) {
				t.Fatalf("err = %v, want INVALID_ARGUMENT %q", err, MessageRestart)
			}
		})
	}
}

// Three failures start the long cooldown, and no request reaches the cluster
// during it. NO_LIVE_IDENTITY reaches no cluster and starts none.
func TestCooldown(t *testing.T) {
	f := small()
	f.errs[NodesGVR] = errRefused
	r, o, c := newReader(t, DefaultLimits(), map[string]*fakeCluster{tenant + "/" + clusterID: f})
	get := func() error {
		_, err := r.GetNodeResources(t.Context(), &cloudv1.GetNodeResourcesRequest{TenantId: tenant, ClusterId: clusterID})
		return err
	}
	steps := []struct {
		advance   time.Duration
		reaches   bool
		reason    string
		retryHint bool
	}{
		{reaches: true, reason: ReasonUnreachable},
		{advance: 10 * time.Second, reaches: false, reason: ReasonUnreachable, retryHint: true}, // cooling 30s
		{advance: 21 * time.Second, reaches: true, reason: ReasonUnreachable},
		{advance: 31 * time.Second, reaches: true, reason: ReasonUnreachable}, // third failure: 2 minutes
		{advance: 31 * time.Second, reaches: false, reason: ReasonUnreachable, retryHint: true},
		{advance: 60 * time.Second, reaches: false, reason: ReasonUnreachable, retryHint: true},
		{advance: 30 * time.Second, reaches: true, reason: ReasonUnreachable},
	}
	for i, s := range steps {
		c.advance(s.advance)
		before := f.callCount()
		err := get()
		if reasonOf(err) != s.reason {
			t.Fatalf("step %d: err = %v", i, err)
		}
		if reached := f.callCount() > before; reached != s.reaches {
			t.Fatalf("step %d: reached the cluster = %v, want %v", i, reached, s.reaches)
		}
		var le *Error
		if errors.As(err, &le); s.retryHint && le.RetryAfter <= 0 {
			t.Fatalf("step %d: no retry hint", i)
		}
	}
	// A success clears the failures.
	c.advance(3 * time.Minute)
	f.mu.Lock()
	delete(f.errs, NodesGVR)
	f.mu.Unlock()
	if err := get(); err != nil {
		t.Fatal(err)
	}

	o.err = &Error{Reason: ReasonNoLiveIdentity, Message: MessageNoLiveIdentity}
	r2, _, c2 := newReader(t, DefaultLimits(), nil)
	r2.open = o
	for range 3 {
		c2.advance(time.Second)
		if _, err := r2.GetNodeResources(t.Context(), &cloudv1.GetNodeResourcesRequest{TenantId: tenant, ClusterId: clusterID}); reasonOf(err) != ReasonNoLiveIdentity {
			t.Fatalf("err = %v", err)
		}
	}
	if r2.guard.cooling(buildKey{tenant, clusterID, kindNodes}, c2.now()) != nil {
		t.Fatal("NO_LIVE_IDENTITY must not cool")
	}
}

func TestSingleflight(t *testing.T) {
	f := small()
	f.gate = make(chan struct{})
	r, o, _ := newReader(t, DefaultLimits(), map[string]*fakeCluster{tenant + "/" + clusterID: f})
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = r.GetNodeResources(t.Context(), &cloudv1.GetNodeResourcesRequest{TenantId: tenant, ClusterId: clusterID})
		})
	}
	time.Sleep(50 * time.Millisecond) // let them all wait on the one build
	close(f.gate)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if o.opens != 1 {
		t.Fatalf("opens = %d, want one build for five requests", o.opens)
	}
}

func TestBusy(t *testing.T) {
	t.Run("builds per cluster per minute", func(t *testing.T) {
		lim := DefaultLimits()
		lim.Reuse = 5 * time.Second // with the 15s reuse four a minute is all a cluster can ask
		r, _, c := newReader(t, lim, map[string]*fakeCluster{tenant + "/" + clusterID: small()})
		for i := range 5 {
			c.advance(6 * time.Second) // past the reuse; five within a minute
			_, err := r.GetNodeResources(t.Context(), &cloudv1.GetNodeResourcesRequest{TenantId: tenant, ClusterId: clusterID})
			if i < 3 && err != nil {
				t.Fatalf("build %d: %v", i, err)
			}
			if i == 4 && reasonOf(err) != ReasonBusy {
				t.Fatalf("build %d: err = %v, want BUSY", i, err)
			}
		}
	})
	t.Run("builds per tenant per minute", func(t *testing.T) {
		clusters := map[string]*fakeCluster{}
		for i := range 11 {
			clusters[fmt.Sprintf("%s/c%d", tenant, i)] = small()
		}
		r, _, _ := newReader(t, DefaultLimits(), clusters)
		var err error
		for i := range 11 {
			_, err = r.GetNodeResources(t.Context(), &cloudv1.GetNodeResourcesRequest{TenantId: tenant, ClusterId: fmt.Sprintf("c%d", i)})
		}
		if reasonOf(err) != ReasonBusy {
			t.Fatalf("11th build: %v", err)
		}
	})
	t.Run("fleet in flight", func(t *testing.T) {
		slow := small()
		slow.gate = make(chan struct{})
		lim := DefaultLimits()
		lim.FleetInFlight, lim.FleetWait = 1, 20*time.Millisecond
		r, _, _ := newReader(t, lim, map[string]*fakeCluster{tenant + "/slow": slow, tenant + "/fast": small()})
		done := make(chan error)
		go func() {
			_, err := r.GetNodeResources(t.Context(), &cloudv1.GetNodeResourcesRequest{TenantId: tenant, ClusterId: "slow"})
			done <- err
		}()
		time.Sleep(20 * time.Millisecond)
		_, err := r.GetNodeResources(t.Context(), &cloudv1.GetNodeResourcesRequest{TenantId: tenant, ClusterId: "fast"})
		if reasonOf(err) != ReasonBusy {
			t.Fatalf("err = %v, want BUSY", err)
		}
		close(slow.gate)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// A view within the reuse window serves a new request; the byte bound
// evicts the least recently used build, whose tokens then restart.
func TestReuseAndEviction(t *testing.T) {
	lim := DefaultLimits()
	clusters := map[string]*fakeCluster{tenant + "/a": many(10), tenant + "/b": many(10)}
	r, o, c := newReader(t, lim, clusters)
	req := func(cluster, token string) (*cloudv1.ListNodeGroupNodesResponse, error) {
		q := listReq(token)
		q.ClusterId = cluster
		return r.ListNodeGroupNodes(t.Context(), q)
	}
	a, err := req("a", "")
	if err != nil {
		t.Fatal(err)
	}
	c.advance(10 * time.Second)
	if _, err := req("a", ""); err != nil || o.opens != 1 {
		t.Fatalf("within 15s the build is reused: opens = %d, %v", o.opens, err)
	}
	c.advance(6 * time.Second)
	if _, err := req("a", ""); err != nil || o.opens != 2 {
		t.Fatalf("after 15s a new build: opens = %d, %v", o.opens, err)
	}
	// Bound the store to about one build: b's build evicts a's.
	r.store.lim.ViewBytes = r.store.size()
	if _, err := req("b", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := req("a", a.GetNextPageToken()); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("an evicted view's token: %v", err)
	}
	if r.store.size() > r.store.lim.ViewBytes {
		t.Fatalf("store %d over its bound %d", r.store.size(), r.store.lim.ViewBytes)
	}
}

func TestSortNodes(t *testing.T) {
	used := func(name string, cpu int64, ready bool) *cloudv1.NodeGroupNode {
		n := &cloudv1.NodeGroupNode{Name: name, AllocatableCpuMillis: 1000, RequestedCpuMillis: cpu, Ready: ready, PodCount: int32(cpu / 100)}
		if cpu >= 0 {
			v := cpu
			n.UsedCpuMillis = &v
		}
		return n
	}
	tests := []struct {
		name string
		by   cloudv1.NodeSortBy
		desc bool
		want string
	}{
		{name: "name", by: cloudv1.NodeSortBy_NODE_SORT_BY_NAME, want: "a b c d"},
		{name: "name desc is still by name for ties only", by: cloudv1.NodeSortBy_NODE_SORT_BY_NAME, desc: true, want: "a b c d"},
		{name: "readiness: not ready first", by: cloudv1.NodeSortBy_NODE_SORT_BY_READINESS, want: "b d a c"},
		{name: "cpu requested", by: cloudv1.NodeSortBy_NODE_SORT_BY_CPU_REQUESTED_PERCENT, want: "d a b c"},
		{name: "cpu requested desc", by: cloudv1.NodeSortBy_NODE_SORT_BY_CPU_REQUESTED_PERCENT, desc: true, want: "c b a d"},
		{name: "cpu used: without usage last", by: cloudv1.NodeSortBy_NODE_SORT_BY_CPU_USED_PERCENT, want: "a b c d"},
		{name: "cpu used desc: without usage still last", by: cloudv1.NodeSortBy_NODE_SORT_BY_CPU_USED_PERCENT, desc: true, want: "c b a d"},
		{name: "pod count", by: cloudv1.NodeSortBy_NODE_SORT_BY_POD_COUNT, want: "d a b c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := []*cloudv1.NodeGroupNode{used("c", 900, true), used("a", 100, true), used("d", -1, false), used("b", 500, false)}
			rows[2].RequestedCpuMillis, rows[2].PodCount = 0, 0
			sortNodes(rows, tt.by, tt.desc)
			var got []string
			for _, r := range rows {
				got = append(got, r.GetName())
			}
			if strings.Join(got, " ") != tt.want {
				t.Fatalf("order = %v, want %s", got, tt.want)
			}
		})
	}
}

func TestToConnect(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code connect.Code
		want string
	}{
		{name: "unreachable", err: newError(ReasonUnreachable, "x"), code: connect.CodeUnavailable, want: ReasonUnreachable},
		{name: "no identity", err: &Error{Reason: ReasonNoLiveIdentity, Message: MessageNoLiveIdentity}, code: connect.CodeUnavailable, want: ReasonNoLiveIdentity},
		{name: "auth required", err: &Error{Reason: ReasonAuthRequired, Message: MessageAuthRequired}, code: connect.CodeFailedPrecondition, want: ReasonAuthRequired},
		{name: "connect error kept", err: errRestart(), code: connect.CodeInvalidArgument},
		{name: "other", err: errors.New("boom"), code: connect.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ToConnect(tt.err)
			if connect.CodeOf(err) != tt.code || ReasonOf(err) != tt.want {
				t.Fatalf("err = %v (reason %q)", err, ReasonOf(err))
			}
		})
	}
}
