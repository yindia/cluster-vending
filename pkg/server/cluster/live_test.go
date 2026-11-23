package cluster

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	liveread "github.com/yindia/cluster-vending/pkg/live"
)

// fakeLive is the live reader as the API sees it.
type fakeLive struct {
	calls int
	err   error
}

func (f *fakeLive) ListNodeGroupNodes(_ context.Context, req *cloudv1.ListNodeGroupNodesRequest) (*cloudv1.ListNodeGroupNodesResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &cloudv1.ListNodeGroupNodesResponse{Nodes: []*cloudv1.NodeGroupNode{{Name: "n-1"}}, Total: 1, Complete: true}, nil
}

func (f *fakeLive) GetNodeResources(_ context.Context, _ *cloudv1.GetNodeResourcesRequest) (*cloudv1.GetNodeResourcesResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &cloudv1.GetNodeResourcesResponse{Cluster: &cloudv1.NodeResourceTotals{Nodes: 1}, Complete: true}, nil
}

// With the live reader deployed the node RPCs go to it, after the API's own
// tenant and cluster checks; without authentication they are refused
// unless live.allowWithoutAuth (FAILED_PRECONDITION, PROP-02 7.5).
func TestLiveNodeRPCs(t *testing.T) {
	tests := []struct {
		name      string
		refused   bool
		liveErr   error
		tenant    string
		group     string
		code      connect.Code
		reason    string
		wantCalls int
	}{
		{name: "served live", tenant: worldTenant, group: systemNodeGroup, wantCalls: 1},
		{name: "auth none without allowWithoutAuth", refused: true, tenant: worldTenant, group: systemNodeGroup,
			code: connect.CodeFailedPrecondition, reason: liveread.ReasonAuthRequired},
		{name: "foreign tenant", tenant: "other", group: systemNodeGroup, code: connect.CodeNotFound},
		{name: "live reader down", tenant: worldTenant, group: systemNodeGroup, liveErr: errors.New("dial tcp: connection refused"),
			code: connect.CodeUnavailable, reason: liveread.ReasonLiveReaderUnavailable, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{}, nil)
			fake := &fakeLive{err: tt.liveErr}
			WithLive(fake, tt.refused)(w.svc)
			res, err := w.svc.ListNodeGroupNodes(t.Context(), &cloudv1.ListNodeGroupNodesRequest{TenantId: tt.tenant, ClusterId: w.row.ID,
				NodeGroup: tt.group, SortBy: cloudv1.NodeSortBy_NODE_SORT_BY_CPU_USED_PERCENT})
			if !answered(err, tt.code) {
				t.Fatalf("ListNodeGroupNodes: %v, want %v", err, tt.code)
			}
			if liveread.ReasonOf(err) != tt.reason {
				t.Fatalf("reason = %q, want %q", liveread.ReasonOf(err), tt.reason)
			}
			if tt.code == 0 && res.GetTotal() != 1 {
				t.Fatalf("response = %v", res)
			}
			nr, err := w.svc.GetNodeResources(t.Context(), &cloudv1.GetNodeResourcesRequest{TenantId: tt.tenant, ClusterId: w.row.ID})
			if !answered(err, tt.code) {
				t.Fatalf("GetNodeResources: %v, want %v", err, tt.code)
			}
			if tt.code == 0 && nr.GetCluster().GetNodes() != 1 {
				t.Fatalf("response = %v", nr)
			}
			if fake.calls != 2*tt.wantCalls {
				t.Fatalf("live reader calls = %d, want %d", fake.calls, 2*tt.wantCalls)
			}
		})
	}
	t.Run("unknown node group", func(t *testing.T) {
		w := newWorld(t, interceptor.Funcs{}, nil)
		fake := &fakeLive{}
		WithLive(fake, false)(w.svc)
		_, err := w.svc.ListNodeGroupNodes(t.Context(), &cloudv1.ListNodeGroupNodesRequest{TenantId: worldTenant, ClusterId: w.row.ID, NodeGroup: "nope"})
		if connect.CodeOf(err) != connect.CodeNotFound || fake.calls != 0 {
			t.Fatalf("err = %v, calls %d", err, fake.calls)
		}
	})
}

// answered: err is the code want, or nil for want 0.
func answered(err error, want connect.Code) bool {
	if want == 0 {
		return err == nil
	}
	return connect.CodeOf(err) == want
}
