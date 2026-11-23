package live

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// small is a cluster of three grouped nodes and one of no group, pods of
// every kind (init containers, a finished pod, one without requests, one on
// a node not in the read), a NodeClaim and node metrics.
func small() *fakeCluster {
	return newFake().
		add(NodesGVR,
			node("sys-1", SystemGroup, true, "2", "8Gi", t0.Add(-3*time.Hour)),
			node("web-1", "web", true, "4", "16Gi", t0.Add(-2*time.Hour)),
			node("web-2", "web", false, "4", "16Gi", t0.Add(-time.Hour)),
			node("stray", "", true, "1", "1Gi", t0)).
		add(PodsGVR,
			pod("a", "web-1", "Running", nil, ctr{"500m", "1Gi"}, ctr{"250m", "512Mi"}),
			// init 2 CPUs beats the containers' 1: requested 2 CPUs.
			pod("b", "web-1", "Pending", []ctr{{"2", "256Mi"}}, ctr{"1", "1Gi"}),
			pod("done", "web-1", "Succeeded", nil, ctr{"8", "8Gi"}),
			pod("bare", "sys-1", "Running", nil, ctr{}),
			pod("orphan", "gone-node", "Running", nil, ctr{"1", "1Gi"})).
		add(NodeClaimsGVR, map[string]any{"apiVersion": "karpenter.sh/v1", "kind": "NodeClaim",
			"metadata": map[string]any{"name": "web-abc"}, "status": map[string]any{"nodeName": "web-1",
				"conditions": []any{map[string]any{"type": "Initialized", "status": "True"}, map[string]any{"type": "Ready", "status": "True"}}}}).
		add(NodeMetricsGVR, nodeMetrics("web-1", "1500m", "4Gi"), nodeMetrics("sys-1", "100m", "1Gi"))
}

func rowNamed(b *nodeBuild, name string) *cloudv1.NodeGroupNode {
	for _, r := range b.rows {
		if r.node.GetName() == name {
			return r.node
		}
	}
	return nil
}

func TestBuildNodes(t *testing.T) {
	tests := []struct {
		name      string
		cluster   func() *fakeCluster
		wantUsage bool
		notices   []string
	}{
		{name: "with node metrics", cluster: small, wantUsage: true, notices: []string{
			"1 pods are bound to nodes not in the read (the node list and the pod list are separate reads)",
			"1 nodes belong to no node group of the platform and are left out"}},
		{name: "metrics.k8s.io not served", cluster: func() *fakeCluster {
			f := small()
			f.errs[NodeMetricsGVR] = apierrors.NewNotFound(schema.GroupResource{Group: "metrics.k8s.io", Resource: "nodes"}, "")
			return f
		}, notices: []string{NoticeNoUsage,
			"1 pods are bound to nodes not in the read (the node list and the pod list are separate reads)",
			"1 nodes belong to no node group of the platform and are left out"}},
		{name: "metrics list fails", cluster: func() *fakeCluster {
			f := small()
			f.errs[NodeMetricsGVR] = apierrors.NewServiceUnavailable("metrics-server down")
			return f
		}, notices: []string{NoticeNoUsage,
			"1 pods are bound to nodes not in the read (the node list and the pod list are separate reads)",
			"1 nodes belong to no node group of the platform and are left out"}},
		{name: "no Karpenter (NodeClaims not served)", cluster: func() *fakeCluster {
			f := small()
			f.errs[NodeClaimsGVR] = apierrors.NewNotFound(schema.GroupResource{Group: "karpenter.sh", Resource: "nodeclaims"}, "")
			return f
		}, wantUsage: true, notices: []string{
			"1 pods are bound to nodes not in the read (the node list and the pod list are separate reads)",
			"1 nodes belong to no node group of the platform and are left out"}},
	}
	lim := DefaultLimits()
	lim.PageLimit = 2 // every list takes several pages
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := buildNodes(t.Context(), tt.cluster(), lim, t0)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(b.notices, tt.notices) {
				t.Fatalf("notices = %q", b.notices)
			}
			if len(b.rows) != 3 {
				t.Fatalf("rows = %d", len(b.rows))
			}
			web1 := rowNamed(b, "web-1")
			// a: 750m, 1.5Gi; b: max(init 2, containers 1) = 2 CPUs, max(256Mi, 1Gi) = 1Gi; done: not counted.
			if web1.GetRequestedCpuMillis() != 2750 || web1.GetRequestedMemoryBytes() != 2560<<20 || web1.GetPodCount() != 2 {
				t.Fatalf("web-1 = %v", web1)
			}
			if web1.GetAllocatableCpuMillis() != 4000 || web1.GetAllocatableMemoryBytes() != 16<<30 || web1.GetAllocatablePods() != 58 {
				t.Fatalf("web-1 allocatable = %v", web1)
			}
			if sys := rowNamed(b, "sys-1"); sys.GetPodsWithoutRequests() != 1 || sys.GetPodCount() != 1 {
				t.Fatalf("sys-1 = %v", sys)
			}
			if (web1.UsedCpuMillis != nil) != tt.wantUsage || tt.wantUsage && (web1.GetUsedCpuMillis() != 1500 || web1.GetUsedMemoryBytes() != 4<<30) {
				t.Fatalf("usage = %v %v", web1.UsedCpuMillis, web1.UsedMemoryBytes)
			}
			if rowNamed(b, "web-2").UsedCpuMillis != nil {
				t.Fatal("a node without metrics has no usage")
			}
			if len(b.groups) != 2 || b.groups[0].GetNodeGroup() != SystemGroup || b.groups[1].GetNodeGroup() != "web" {
				t.Fatalf("groups = %v", b.groups)
			}
			web := b.groups[1]
			if web.GetNodes() != 2 || web.GetReadyNodes() != 1 || web.GetAllocatableCpuMillis() != 8000 || web.GetRequestedCpuMillis() != 2750 || web.GetPodCount() != 2 {
				t.Fatalf("web totals = %v", web)
			}
			c := b.cluster
			if c.GetNodes() != 3 || c.GetAllocatableCpuMillis() != 10000 || c.GetPodsWithoutRequests() != 1 {
				t.Fatalf("cluster totals = %v", c)
			}
			if (c.UsedCpuMillis != nil) != tt.wantUsage || tt.wantUsage && c.GetUsedCpuMillis() != 1600 {
				t.Fatalf("cluster usage = %v", c.UsedCpuMillis)
			}
			if b.bytes <= 0 {
				t.Fatal("a build has a size")
			}
		})
	}
	t.Run("the NodeClaim is on its node", func(t *testing.T) {
		b, err := buildNodes(t.Context(), small(), lim, t0)
		if err != nil {
			t.Fatal(err)
		}
		if r := rowNamed(b, "web-1"); r.GetNodeclaimName() != "web-abc" || r.GetNodeclaimPhase() != "Initialized" || !r.GetNodeclaimReady() {
			t.Fatalf("web-1 claim = %v", r)
		}
	})
}

// One row per cap and failure of 5.6 and 5.9: each is a live Error with its
// reason, never rows.
func TestBuildNodesFails(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("no"))
	tests := []struct {
		name   string
		setup  func(f *fakeCluster, lim *Limits)
		reason string
		ok     bool
	}{
		{name: "rows over the cap", setup: func(_ *fakeCluster, lim *Limits) { lim.MaxNodeRows = 2 }, reason: ReasonLimitExceeded},
		{name: "pages over the cap", setup: func(_ *fakeCluster, lim *Limits) { lim.PageLimit, lim.MaxPages = 1, 3 }, reason: ReasonLimitExceeded},
		{name: "deadline before the first page", setup: func(f *fakeCluster, lim *Limits) { f.hang, lim.NodesDeadline = true, 20*time.Millisecond }, reason: ReasonUnreachable},
		{name: "deadline after the first page", setup: func(f *fakeCluster, lim *Limits) {
			f.hangAfter, lim.PageLimit, lim.NodesDeadline = true, 1, 20*time.Millisecond
		}, reason: ReasonLimitExceeded},
		{name: "a token expires once: restarted", setup: func(f *fakeCluster, lim *Limits) { f.expire, lim.PageLimit = 1, 1 }, ok: true},
		{name: "a token expires twice: list expired", setup: func(f *fakeCluster, lim *Limits) { f.expire, lim.PageLimit = 2, 1 }, reason: ReasonUnreachable},
		{name: "403", setup: func(f *fakeCluster, _ *Limits) { f.errs[NodesGVR] = forbidden }, reason: ReasonCredentialsRejected},
		{name: "401", setup: func(f *fakeCluster, _ *Limits) { f.errs[NodesGVR] = apierrors.NewUnauthorized("token expired") }, reason: ReasonCredentialsRejected},
		{name: "connection refused", setup: func(f *fakeCluster, _ *Limits) { f.errs[NodesGVR] = errRefused }, reason: ReasonUnreachable},
		{name: "pods fail after the nodes", setup: func(f *fakeCluster, _ *Limits) { f.errs[PodsGVR] = errRefused }, reason: ReasonUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, lim := small(), DefaultLimits()
			tt.setup(f, &lim)
			ctx, cancel := context.WithTimeout(t.Context(), lim.NodesDeadline)
			defer cancel()
			b, err := buildNodes(ctx, f, lim, t0)
			if tt.ok {
				if err != nil || len(b.rows) != 3 {
					t.Fatalf("build = %v, %v", b, err)
				}
				return
			}
			var le *Error
			if !errors.As(err, &le) || le.Reason != tt.reason {
				t.Fatalf("err = %v, want reason %s", err, tt.reason)
			}
			if tt.name == "a token expires twice: list expired" && le.Message != "list expired" {
				t.Fatalf("message = %q", le.Message)
			}
		})
	}
}
