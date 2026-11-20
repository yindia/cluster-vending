package cluster

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/aws/smithy-go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/results/resultstest"
)

var nodesAt = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// nodesWorld is a cluster whose node rows are in a results bucket (an
// in-memory S3 with ETag-conditional reads).
type nodesWorld struct {
	world
	svc    *ClusterService
	s3     *resultstest.S3
	bucket string
}

func newNodesWorld(t *testing.T) *nodesWorld {
	t.Helper()
	w := newWorld(t, interceptor.Funcs{}, nil)
	nw := &nodesWorld{world: w}
	nw.bucket = crossplaneaws.ResultsBucketName("prod-1a2b3c4d", "444455556666", "us-east-1", w.row.ID)
	nw.s3 = resultstest.New(nw.bucket)
	nw.svc = NewClusterService(w.svc.deps, WithResults(NewS3Results(func(*model.CloudAccount, string) (results.S3API, error) { return nw.s3, nil }, results.NewCache(0))))
	// The status names the groups the cluster has.
	st := &cloudv1.ClusterStatus{NodeGroups: []*cloudv1.NodeGroupStatus{{Name: "platform", PlatformManaged: true}, {Name: "batch"}, {Name: "gpu"}}}
	raw, err := protojson.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.Clusters().SetStatus(t.Context(), worldTenant, w.row.ID, w.cluster(t).ETag, raw); err != nil {
		t.Fatal(err)
	}
	return nw
}

// node n of a group: every fifth is not ready.
func nodeRow(group string, i int) *cloudv1.NodeGroupNode {
	return &cloudv1.NodeGroupNode{Name: fmt.Sprintf("%s-node-%04d", group, i), InstanceType: "m7i.large", Zone: "us-east-1a",
		Ready: i%5 != 0, CreatedAt: timestamppb.New(nodesAt)}
}

func groupOf(name string, n int) *cloudv1.NodeGroupNodes {
	g := &cloudv1.NodeGroupNodes{Group: name}
	for i := range n {
		g.Nodes = append(g.Nodes, nodeRow(name, i))
	}
	return g
}

// putNodes writes the shards and records the summary on the cluster, as the
// controller does.
func (w *nodesWorld) putNodes(t *testing.T, opts []results.ShardOption, groups ...*cloudv1.NodeGroupNodes) {
	t.Helper()
	shards := map[string]proto.Message{}
	total := 0
	for _, g := range groups {
		shards[g.Group] = g
		total += len(g.Nodes)
	}
	res, err := results.NewWriter(w.s3).PutSharded(t.Context(), w.bucket, results.Nodes, shards, nodesAt, opts...)
	if err != nil {
		t.Fatal(err)
	}
	ref := model.ResultRef{Kind: string(results.Nodes), Bucket: w.bucket, Key: res.Key, ETag: res.ETag, CollectedAt: nodesAt, Total: int32(total)}
	if err := w.store.Clusters().SetResultRef(t.Context(), worldTenant, w.row.ID, ref); err != nil {
		t.Fatal(err)
	}
}

func (w *nodesWorld) list(t *testing.T, req *cloudv1.ListNodeGroupNodesRequest) (*cloudv1.ListNodeGroupNodesResponse, error) {
	t.Helper()
	req.TenantId, req.ClusterId = worldTenant, w.row.ID
	if req.NodeGroup == "" {
		req.NodeGroup = "batch"
	}
	return w.svc.ListNodeGroupNodes(t.Context(), req)
}

func (w *nodesWorld) mustList(t *testing.T, req *cloudv1.ListNodeGroupNodesRequest) *cloudv1.ListNodeGroupNodesResponse {
	t.Helper()
	res, err := w.list(t, req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestNodeGroupNodesPagesAThousandNodesFromOneShard(t *testing.T) {
	w := newNodesWorld(t)
	var others []*cloudv1.NodeGroupNodes
	for i := range 9 { // 10 groups in all, one of them with 1,000 nodes
		others = append(others, groupOf(fmt.Sprintf("g%d", i), 3))
	}
	w.putNodes(t, nil, append(others, groupOf("batch", 1000))...)
	gets := w.s3.Calls["GetObject"]

	req := &cloudv1.ListNodeGroupNodesRequest{PageSize: 500}
	var names []string
	pages := 0
	for {
		res := w.mustList(t, req)
		pages++
		if res.GetTotal() != 1000 || !res.GetComplete() || len(res.GetNotices()) != 0 || !res.GetCollectedAt().AsTime().Equal(nodesAt) {
			t.Fatalf("page %d header = total %d complete %v notices %v at %v", pages, res.GetTotal(), res.GetComplete(), res.GetNotices(), res.GetCollectedAt())
		}
		for _, n := range res.GetNodes() {
			names = append(names, n.GetName())
		}
		if res.GetNextPageToken() == "" {
			break
		}
		if len(res.GetNextPageToken()) > 8 || strings.Trim(res.GetNextPageToken(), "0123456789") != "" {
			t.Fatalf("token %q does not fit page_token (at most 8 digits)", res.GetNextPageToken())
		}
		req.PageToken = res.GetNextPageToken()
	}
	if pages != 2 || len(names) != 1000 || names[0] != "batch-node-0000" || names[999] != "batch-node-0999" {
		t.Errorf("pages %d, nodes %d, first %q", pages, len(names), names[0])
	}
	// Two pages are each the index and the one shard of the group: never
	// another group's.
	if got := w.s3.Calls["GetObject"] - gets; got != 4 {
		t.Errorf("GetObject calls = %d, want 4 (index and the group's shard, twice)", got)
	}
}

func TestNodeGroupNodesDefaultsAndCapsThePageSize(t *testing.T) {
	w := newNodesWorld(t)
	w.putNodes(t, nil, groupOf("batch", 700))
	if res := w.mustList(t, &cloudv1.ListNodeGroupNodesRequest{}); len(res.GetNodes()) != 100 || res.GetNextPageToken() == "" {
		t.Errorf("default page = %d, next %q", len(res.GetNodes()), res.GetNextPageToken())
	}
	if res := w.mustList(t, &cloudv1.ListNodeGroupNodesRequest{PageSize: 5000}); len(res.GetNodes()) != 500 {
		t.Errorf("capped page = %d", len(res.GetNodes()))
	}
}

func TestNodeGroupNodesFilterAndSearch(t *testing.T) {
	w := newNodesWorld(t)
	w.putNodes(t, nil, groupOf("batch", 100))
	for name, tt := range map[string]struct {
		req   *cloudv1.ListNodeGroupNodesRequest
		total int32
	}{
		"all":                {&cloudv1.ListNodeGroupNodesRequest{Filter: cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_ALL}, 100},
		"unspecified is all": {&cloudv1.ListNodeGroupNodesRequest{}, 100},
		"ready":              {&cloudv1.ListNodeGroupNodesRequest{Filter: cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_READY}, 80},
		"not ready":          {&cloudv1.ListNodeGroupNodesRequest{Filter: cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_NOT_READY}, 20},
		"name":               {&cloudv1.ListNodeGroupNodesRequest{NameContains: "node-004"}, 10},
		"name, any case":     {&cloudv1.ListNodeGroupNodesRequest{NameContains: "NODE-0042"}, 1},
		"name and not ready": {&cloudv1.ListNodeGroupNodesRequest{NameContains: "node-00", Filter: cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_NOT_READY}, 20},
		"no match":           {&cloudv1.ListNodeGroupNodesRequest{NameContains: "zzz"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			res := w.mustList(t, tt.req)
			if res.GetTotal() != tt.total || int32(len(res.GetNodes())) != tt.total {
				t.Errorf("total %d, rows %d, want %d", res.GetTotal(), len(res.GetNodes()), tt.total)
			}
			for _, n := range res.GetNodes() {
				if tt.req.GetFilter() == cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_NOT_READY && n.GetReady() {
					t.Errorf("%s is ready", n.GetName())
				}
				if tt.req.GetFilter() == cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_READY && !n.GetReady() {
					t.Errorf("%s is not ready", n.GetName())
				}
			}
		})
	}
}

// A token belongs to the shard's object, the filter and the search.
func TestNodeGroupNodesPageTokenIsBound(t *testing.T) {
	w := newNodesWorld(t)
	w.putNodes(t, nil, groupOf("batch", 300))
	first := w.mustList(t, &cloudv1.ListNodeGroupNodesRequest{PageSize: 100})
	if first.GetNextPageToken() == "" {
		t.Fatal("no next page")
	}
	bad := func(what string, req *cloudv1.ListNodeGroupNodesRequest) {
		t.Helper()
		req.PageToken = first.GetNextPageToken()
		if _, err := w.list(t, req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", what, err)
		}
	}
	bad("another filter", &cloudv1.ListNodeGroupNodesRequest{PageSize: 100, Filter: cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_READY})
	bad("another search", &cloudv1.ListNodeGroupNodesRequest{PageSize: 100, NameContains: "node"})
	bad("another group", &cloudv1.ListNodeGroupNodesRequest{PageSize: 100, NodeGroup: "gpu"})
	for _, tok := range []string{"abc", "99999999"} {
		if _, err := w.list(t, &cloudv1.ListNodeGroupNodesRequest{PageToken: tok}); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("token %q: %v, want InvalidArgument", tok, err)
		}
	}
	second := w.mustList(t, &cloudv1.ListNodeGroupNodesRequest{PageSize: 100, PageToken: first.GetNextPageToken()})
	if len(second.GetNodes()) != 100 || second.GetNodes()[0].GetName() != "batch-node-0100" {
		t.Fatalf("second page = %d from %q", len(second.GetNodes()), second.GetNodes()[0].GetName())
	}
	// The group's nodes change (a new shard object): the old token is refused.
	changed := groupOf("batch", 300)
	changed.Nodes[1].Ready = false
	w.putNodes(t, nil, changed)
	if _, err := w.list(t, &cloudv1.ListNodeGroupNodesRequest{PageSize: 100, PageToken: first.GetNextPageToken()}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a token of the previous shard: %v, want InvalidArgument", err)
	}
}

func TestNodeGroupNodesIsTenantScoped(t *testing.T) {
	w := newNodesWorld(t)
	w.putNodes(t, nil, groupOf("batch", 3))
	w.s3.Calls = map[string]int{}
	_, err := w.svc.ListNodeGroupNodes(t.Context(), &cloudv1.ListNodeGroupNodesRequest{TenantId: "other", ClusterId: w.row.ID, NodeGroup: "batch"})
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("another tenant: %v, want NotFound", err)
	}
	if len(w.s3.Calls) != 0 {
		t.Errorf("the bucket was touched: %v", w.s3.Calls)
	}
	// A group the cluster does not have.
	if _, err := w.list(t, &cloudv1.ListNodeGroupNodesRequest{NodeGroup: "ghost"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown group: %v, want NotFound", err)
	}
	// system and platform are the cluster's own.
	for _, g := range []string{"system", "platform"} {
		if _, err := w.list(t, &cloudv1.ListNodeGroupNodesRequest{NodeGroup: g}); err != nil {
			t.Errorf("%s: %v", g, err)
		}
	}
}

func TestNodeGroupNodesRefusesAForgedBucket(t *testing.T) {
	w := newNodesWorld(t)
	w.bucket = crossplaneaws.ResultsBucketName("prod-1a2b3c4d", "999988887777", "us-east-1", w.row.ID)
	w.s3.Set(w.bucket, "placeholder", nil)
	w.putNodes(t, nil, groupOf("batch", 3))
	w.s3.Calls = map[string]int{}
	res := w.mustList(t, &cloudv1.ListNodeGroupNodesRequest{})
	if len(res.GetNodes()) != 0 || len(res.GetNotices()) == 0 || w.s3.Calls["GetObject"] != 0 {
		t.Errorf("res = %v, reads %v", res, w.s3.Calls)
	}
}

func TestNodeGroupNodesNoticesInsteadOfErrors(t *testing.T) {
	for name, tt := range map[string]struct {
		err    error
		notice string
	}{
		"denied":  {&smithy.GenericAPIError{Code: "AccessDenied"}, "access to the results bucket was denied"},
		"gone":    {&smithy.GenericAPIError{Code: "NoSuchKey"}, "none have been stored"},
		"timeout": {context.DeadlineExceeded, "timed out"},
		"other":   {fmt.Errorf("connection reset"), "could not be read"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newNodesWorld(t)
			w.putNodes(t, nil, groupOf("batch", 3))
			w.s3.Fail = func(op, _, _ string) error {
				if op == "GetObject" {
					return tt.err
				}
				return nil
			}
			res, err := w.list(t, &cloudv1.ListNodeGroupNodesRequest{})
			if err != nil {
				t.Fatalf("the call failed: %v", err)
			}
			if len(res.GetNodes()) != 0 || len(res.GetNotices()) == 0 || !strings.Contains(res.GetNotices()[0], tt.notice) {
				t.Errorf("nodes %d, notices %v, want %q", len(res.GetNodes()), res.GetNotices(), tt.notice)
			}
		})
	}
	t.Run("only the shard fails", func(t *testing.T) {
		w := newNodesWorld(t)
		w.putNodes(t, nil, groupOf("batch", 3))
		w.s3.Fail = func(op, _, key string) error {
			if op == "GetObject" && key == results.Nodes.ShardLatestKey("batch") {
				return &smithy.GenericAPIError{Code: "AccessDenied"}
			}
			return nil
		}
		res := w.mustList(t, &cloudv1.ListNodeGroupNodesRequest{})
		if len(res.GetNodes()) != 0 || len(res.GetNotices()) != 1 {
			t.Errorf("res = %v", res)
		}
	})
}

func TestNodeGroupNodesNotCollectedYet(t *testing.T) {
	w := newNodesWorld(t)
	res := w.mustList(t, &cloudv1.ListNodeGroupNodesRequest{})
	if len(res.GetNodes()) != 0 || res.GetCollectedAt() != nil || len(res.GetNotices()) != 1 || !strings.Contains(res.GetNotices()[0], "not been collected") {
		t.Errorf("res = %v", res)
	}
	if w.s3.Calls["GetObject"] != 0 {
		t.Errorf("the bucket was read without a summary")
	}
	// A server with no AWS access says so.
	w.putNodes(t, nil, groupOf("batch", 3))
	bare := NewClusterService(w.world.svc.deps)
	res, err := bare.ListNodeGroupNodes(t.Context(), &cloudv1.ListNodeGroupNodesRequest{TenantId: worldTenant, ClusterId: w.row.ID, NodeGroup: "batch"})
	if err != nil || len(res.GetNotices()) != 1 || !strings.Contains(res.GetNotices()[0], "not set up") {
		t.Errorf("res = %v, %v", res, err)
	}
}

func TestNodeGroupNodesWhenTheControllerCouldNotWrite(t *testing.T) {
	w := newNodesWorld(t)
	st := &cloudv1.ClusterStatus{NodeGroups: []*cloudv1.NodeGroupStatus{{Name: "batch", Nodes: 4, NodesNotice: "scan results are kept in the cluster object: the results bucket is not ready yet"}}}
	raw, _ := protojson.Marshal(st)
	if err := w.store.Clusters().SetStatus(t.Context(), worldTenant, w.row.ID, w.cluster(t).ETag, raw); err != nil {
		t.Fatal(err)
	}
	res := w.mustList(t, &cloudv1.ListNodeGroupNodesRequest{})
	if len(res.GetNodes()) != 0 || len(res.GetNotices()) != 1 || !strings.Contains(res.GetNotices()[0], "results bucket is not ready yet") {
		t.Errorf("res = %v", res)
	}
}

func TestNodeGroupNodesAGroupWithoutNodes(t *testing.T) {
	w := newNodesWorld(t)
	w.putNodes(t, nil, groupOf("gpu", 3))
	res := w.mustList(t, &cloudv1.ListNodeGroupNodesRequest{NodeGroup: "batch"})
	if res.GetTotal() != 0 || len(res.GetNodes()) != 0 || len(res.GetNotices()) != 0 || res.GetCollectedAt() == nil || !res.GetComplete() {
		t.Errorf("res = %v", res)
	}
}

func TestNodeGroupNodesIncompleteCollection(t *testing.T) {
	w := newNodesWorld(t)
	w.putNodes(t, []results.ShardOption{results.Incomplete()}, groupOf("batch", 3))
	res := w.mustList(t, &cloudv1.ListNodeGroupNodesRequest{})
	if res.GetComplete() || len(res.GetNodes()) != 3 || len(res.GetNotices()) != 1 || !strings.Contains(res.GetNotices()[0], "incomplete") {
		t.Errorf("res = %v", res)
	}
}

func TestNodeGroupNodesAMovedSummaryTimeShowsAsCollected(t *testing.T) {
	w := newNodesWorld(t)
	w.putNodes(t, nil, groupOf("batch", 3))
	row := w.cluster(t)
	ref := *row.ResultRef("nodes")
	ref.CollectedAt = nodesAt.Add(7 * time.Minute) // an unchanged collection moves the summary's time only
	if err := w.store.Clusters().SetResultRef(t.Context(), worldTenant, w.row.ID, ref); err != nil {
		t.Fatal(err)
	}
	if got := w.mustList(t, &cloudv1.ListNodeGroupNodesRequest{}).GetCollectedAt().AsTime(); !got.Equal(nodesAt.Add(7 * time.Minute)) {
		t.Errorf("collected at %v", got)
	}
}

func TestListNodeGroupNodesValidatesTheRequest(t *testing.T) {
	w := newNodesWorld(t)
	for name, req := range map[string]*cloudv1.ListNodeGroupNodesRequest{
		"no group":       {TenantId: worldTenant, ClusterId: w.row.ID},
		"bad group":      {TenantId: worldTenant, ClusterId: w.row.ID, NodeGroup: "../x"},
		"negative size":  {TenantId: worldTenant, ClusterId: w.row.ID, NodeGroup: "batch", PageSize: -1},
		"long token":     {TenantId: worldTenant, ClusterId: w.row.ID, NodeGroup: "batch", PageToken: "123456789"},
		"bad cluster id": {TenantId: worldTenant, ClusterId: "x", NodeGroup: "batch"},
	} {
		if _, err := w.svc.ListNodeGroupNodes(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
}
