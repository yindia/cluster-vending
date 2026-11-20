package cluster

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func TestSortScope(t *testing.T) {
	const (
		byDefault = 1
		byOther   = 2
	)
	tests := []struct {
		name        string
		by          int32
		desc        bool
		defaultDesc bool
		wantScope   bool
	}{
		{"unspecified", 0, false, false, false},
		{"the default key said out loud", byDefault, false, false, false},
		{"the default direction of a descending key", 0, true, true, false},
		{"another direction", 0, true, false, true},
		{"another key", byOther, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sortScope(tt.by, tt.desc, tt.defaultDesc)
			require.Equal(t, tt.wantScope, got != "", got)
		})
	}
	require.NotEqual(t, sortScope(2, false, false), sortScope(3, false, false), "two keys, two scopes")
	require.NotEqual(t, sortScope(2, false, false), sortScope(2, true, false), "two directions, two scopes")
}

func deprecationsToSort() *cloudv1.ClusterDeprecations {
	f := func(ns, kind, name string, removed bool) *cloudv1.DeprecationFinding {
		return &cloudv1.DeprecationFinding{Namespace: ns, Kind: kind, Name: name, Removed: removed}
	}
	// As the controller stores them: removed first.
	return &cloudv1.ClusterDeprecations{TargetVersion: "1.37", Total: 5, Removed: 2, Findings: []*cloudv1.DeprecationFinding{
		f("web", "Ingress", "r1", true), f("api", "PodSecurityPolicy", "r2", true),
		f("", "CronJob", "d1", false), f("web", "CronJob", "d2", false), f("api", "Ingress", "d3", false)}}
}

func listDeprecations(t *testing.T, w *resultsWorld, req *cloudv1.ListClusterDeprecationsRequest) (*cloudv1.ListClusterDeprecationsResponse, error) {
	t.Helper()
	req.TenantId, req.ClusterId = worldTenant, w.row.ID
	return w.svc.ListClusterDeprecations(t.Context(), req)
}

func TestListClusterDeprecationsSorts(t *testing.T) {
	w := newResultsWorld(t)
	w.put(t, results.Deprecations, deprecationsToSort())
	tests := []struct {
		name string
		req  *cloudv1.ListClusterDeprecationsRequest
		want string
	}{
		{"default", &cloudv1.ListClusterDeprecationsRequest{}, "r1 r2 d1 d2 d3"},
		{"removed first, said out loud", &cloudv1.ListClusterDeprecationsRequest{SortBy: cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_REMOVED_FIRST, SortOrder: cloudv1.SortOrder_SORT_ORDER_ASC}, "r1 r2 d1 d2 d3"},
		{"removed last", &cloudv1.ListClusterDeprecationsRequest{SortOrder: cloudv1.SortOrder_SORT_ORDER_DESC}, "d1 d2 d3 r1 r2"},
		{"namespace", &cloudv1.ListClusterDeprecationsRequest{SortBy: cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_NAMESPACE}, "d1 d3 r2 d2 r1"},
		{"namespace, descending", &cloudv1.ListClusterDeprecationsRequest{SortBy: cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_NAMESPACE, SortOrder: cloudv1.SortOrder_SORT_ORDER_DESC}, "r1 d2 r2 d3 d1"},
		{"kind", &cloudv1.ListClusterDeprecationsRequest{SortBy: cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_KIND}, "d1 d2 d3 r1 r2"},
		{"kind with only removed", &cloudv1.ListClusterDeprecationsRequest{SortBy: cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_KIND, OnlyRemoved: true, SortOrder: cloudv1.SortOrder_SORT_ORDER_DESC}, "r2 r1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := listDeprecations(t, w, tt.req)
			require.NoError(t, err)
			var got []string
			for _, f := range res.GetFindings() {
				got = append(got, f.GetName())
			}
			require.Equal(t, tt.want, strings.Join(got, " "))
			require.EqualValues(t, 5, res.GetTotal(), "the summary does not depend on the sort")
		})
	}
}

// A token is good for the sort it was made with and for the object it was made
// from; an unsorted request's tokens are the ones it always made.
func TestDeprecationTokensAreScopedToTheSort(t *testing.T) {
	w := newResultsWorld(t)
	etag := w.put(t, results.Deprecations, deprecationsToSort())

	plain, err := listDeprecations(t, w, &cloudv1.ListClusterDeprecationsRequest{PageSize: 2})
	require.NoError(t, err)
	require.Equal(t, etagToken(etag, 2), plain.GetNextPageToken(), "an unsorted list's token is as before")
	saidOutLoud, err := listDeprecations(t, w, &cloudv1.ListClusterDeprecationsRequest{PageSize: 2, SortBy: cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_REMOVED_FIRST})
	require.NoError(t, err)
	require.Equal(t, plain.GetNextPageToken(), saidOutLoud.GetNextPageToken())

	byKind := &cloudv1.ListClusterDeprecationsRequest{PageSize: 2, SortBy: cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_KIND}
	first, err := listDeprecations(t, w, byKind)
	require.NoError(t, err)
	require.NotEmpty(t, first.GetNextPageToken())
	require.Regexp(t, `^[0-9]{1,8}$`, first.GetNextPageToken(), "fits page_token")
	require.NotEqual(t, plain.GetNextPageToken(), first.GetNextPageToken())

	byKind.PageToken = first.GetNextPageToken()
	second, err := listDeprecations(t, w, byKind)
	require.NoError(t, err)
	require.Equal(t, "d3 r1", second.GetFindings()[0].GetName()+" "+second.GetFindings()[1].GetName())

	for name, req := range map[string]*cloudv1.ListClusterDeprecationsRequest{
		"the sorted token on an unsorted list":  {PageSize: 2, PageToken: first.GetNextPageToken()},
		"the unsorted token on a sorted list":   {PageSize: 2, PageToken: plain.GetNextPageToken(), SortBy: cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_KIND},
		"the token of one key on another":       {PageSize: 2, PageToken: first.GetNextPageToken(), SortBy: cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_NAMESPACE},
		"the token of one direction on another": {PageSize: 2, PageToken: first.GetNextPageToken(), SortBy: cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_KIND, SortOrder: cloudv1.SortOrder_SORT_ORDER_DESC},
	} {
		_, err := listDeprecations(t, w, req)
		require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), name)
	}
}

// Without a bucket object the Cluster object's copy answers, with plain
// offsets, and sorts the same way.
func TestDeprecationsOfTheClusterObjectSort(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	scanned(t, w, 2, 3) // namespace web, kind Ingress; names a..e, a and b removed
	list := func(req *cloudv1.ListClusterDeprecationsRequest) (*cloudv1.ListClusterDeprecationsResponse, error) {
		req.TenantId, req.ClusterId = worldTenant, w.row.ID
		return w.svc.ListClusterDeprecations(t.Context(), req)
	}
	res, err := list(&cloudv1.ListClusterDeprecationsRequest{SortOrder: cloudv1.SortOrder_SORT_ORDER_DESC, PageSize: 3})
	require.NoError(t, err)
	require.Equal(t, "cde", names(res.GetFindings()))
	res2, err := list(&cloudv1.ListClusterDeprecationsRequest{SortOrder: cloudv1.SortOrder_SORT_ORDER_DESC, PageSize: 3, PageToken: res.GetNextPageToken()})
	require.NoError(t, err)
	require.Equal(t, "ab", names(res2.GetFindings()))
	// An old client's plain offset is still a token of the unsorted list.
	old, err := list(&cloudv1.ListClusterDeprecationsRequest{PageSize: 2, PageToken: "2"})
	require.NoError(t, err)
	require.Equal(t, "cd", names(old.GetFindings()))
	// but not of a sorted one.
	_, err = list(&cloudv1.ListClusterDeprecationsRequest{PageSize: 2, PageToken: "2", SortOrder: cloudv1.SortOrder_SORT_ORDER_DESC})
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

func recommendationsToSort() *cloudv1.ListClusterRecommendationsResponse {
	r := func(ns, name string, cpu, mem int64, recommended bool) *cloudv1.ResourceRecommendation {
		out := &cloudv1.ResourceRecommendation{Namespace: ns, WorkloadKind: "Deployment", WorkloadName: name, Container: "app"}
		if recommended {
			out.RecommendedRequests = &cloudv1.ResourceAmounts{CpuMillis: 1}
			out.Difference = &cloudv1.ResourceAmounts{CpuMillis: cpu, MemoryBytes: mem}
		}
		return out
	}
	return &cloudv1.ListClusterRecommendationsResponse{Total: 5, Summary: &cloudv1.ClusterRecommendations{Containers: 5, MetricsAvailable: true},
		Recommendations: []*cloudv1.ResourceRecommendation{
			r("a", "small", 100, -900, true), r("a", "none", 0, 0, false), r("b", "shrink", -300, 50, true),
			r("b", "same", 0, 0, true), r("c", "grow", 200, 700, true)}}
}

func recommendationNames(res *cloudv1.ListClusterRecommendationsResponse) string {
	var out []string
	for _, r := range res.GetRecommendations() {
		out = append(out, r.GetWorkloadName())
	}
	return strings.Join(out, " ")
}

func TestListClusterRecommendationsSorts(t *testing.T) {
	w := newResultsWorld(t)
	w.put(t, results.Recommendations, recommendationsToSort())
	const (
		cpu  = cloudv1.RecommendationSortBy_RECOMMENDATION_SORT_BY_CPU_CHANGE
		mem  = cloudv1.RecommendationSortBy_RECOMMENDATION_SORT_BY_MEMORY_CHANGE
		asc  = cloudv1.SortOrder_SORT_ORDER_ASC
		desc = cloudv1.SortOrder_SORT_ORDER_DESC
	)
	tests := []struct {
		name string
		req  *cloudv1.ListClusterRecommendationsRequest
		want string
	}{
		{"default", &cloudv1.ListClusterRecommendationsRequest{}, "small none shrink same grow"},
		{"namespace descending", &cloudv1.ListClusterRecommendationsRequest{SortOrder: desc}, "grow shrink same small none"},
		{"cpu change: the largest first, no recommendation last", &cloudv1.ListClusterRecommendationsRequest{SortBy: cpu}, "shrink grow small same none"},
		{"cpu change ascending", &cloudv1.ListClusterRecommendationsRequest{SortBy: cpu, SortOrder: asc}, "same small grow shrink none"},
		{"memory change", &cloudv1.ListClusterRecommendationsRequest{SortBy: mem}, "small grow shrink same none"},
		{"memory change with a filter", &cloudv1.ListClusterRecommendationsRequest{SortBy: mem, Namespace: "b"}, "shrink same"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.TenantId, tt.req.ClusterId = worldTenant, w.row.ID
			res, err := w.svc.ListClusterRecommendations(t.Context(), tt.req)
			require.NoError(t, err)
			require.Equal(t, tt.want, recommendationNames(res))
		})
	}

	// Tokens: scoped to the sort and to the object.
	req := &cloudv1.ListClusterRecommendationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, PageSize: 2, SortBy: cpu}
	first, err := w.svc.ListClusterRecommendations(t.Context(), req)
	require.NoError(t, err)
	req.PageToken = first.GetNextPageToken()
	second, err := w.svc.ListClusterRecommendations(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "small same", recommendationNames(second))
	for name, bad := range map[string]*cloudv1.ListClusterRecommendationsRequest{
		"another key":    {SortBy: mem},
		"unsorted":       {},
		"another order":  {SortBy: cpu, SortOrder: asc},
		"a plain offset": {SortBy: cpu, PageToken: "2"},
	} {
		bad.TenantId, bad.ClusterId, bad.PageSize = worldTenant, w.row.ID, 2
		if bad.PageToken == "" {
			bad.PageToken = first.GetNextPageToken()
		}
		_, err := w.svc.ListClusterRecommendations(t.Context(), bad)
		require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), name)
	}
}

// The computed path (no bucket object) sorts and pages alike.
func TestComputedRecommendationsSort(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	defer func(clock func() time.Time) { now = clock }(now)
	now = func() time.Time { return sampleStart.Add(48 * time.Hour) }
	recorded(t, w)
	req := &cloudv1.ListClusterRecommendationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, SortOrder: cloudv1.SortOrder_SORT_ORDER_DESC, PageSize: 2}
	first, err := w.svc.ListClusterRecommendations(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "db web", recommendationNames(first))
	req.PageToken = first.GetNextPageToken()
	second, err := w.svc.ListClusterRecommendations(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "agent", recommendationNames(second))
	req.SortOrder = cloudv1.SortOrder_SORT_ORDER_UNSPECIFIED
	_, err = w.svc.ListClusterRecommendations(t.Context(), req)
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

// Until the live reader exists the live lists keep answering from the stored
// results; a sort they cannot do is UNIMPLEMENTED, not a silently unsorted page.
func TestLiveListsRefuseASortTheyCannotDo(t *testing.T) {
	w := newNodesWorld(t)
	base := func() (string, string) { return worldTenant, w.row.ID }
	tenant, id := base()

	nodes := func(by cloudv1.NodeSortBy, order cloudv1.SortOrder) error {
		_, err := w.svc.ListNodeGroupNodes(t.Context(), &cloudv1.ListNodeGroupNodesRequest{TenantId: tenant, ClusterId: id, NodeGroup: "batch", SortBy: by, SortOrder: order})
		return err
	}
	require.NotEqual(t, connect.CodeUnimplemented, connect.CodeOf(nodes(0, 0)))
	require.NotEqual(t, connect.CodeUnimplemented, connect.CodeOf(nodes(cloudv1.NodeSortBy_NODE_SORT_BY_NAME, cloudv1.SortOrder_SORT_ORDER_ASC)))
	for _, by := range []cloudv1.NodeSortBy{cloudv1.NodeSortBy_NODE_SORT_BY_AGE, cloudv1.NodeSortBy_NODE_SORT_BY_READINESS,
		cloudv1.NodeSortBy_NODE_SORT_BY_CPU_REQUESTED_PERCENT, cloudv1.NodeSortBy_NODE_SORT_BY_MEMORY_REQUESTED_PERCENT, cloudv1.NodeSortBy_NODE_SORT_BY_POD_COUNT} {
		require.Equal(t, connect.CodeUnimplemented, connect.CodeOf(nodes(by, 0)), by.String())
	}
	require.Equal(t, connect.CodeUnimplemented, connect.CodeOf(nodes(0, cloudv1.SortOrder_SORT_ORDER_DESC)))

	violations := func(by cloudv1.ViolationSortBy, order cloudv1.SortOrder) error {
		_, err := w.svc.ListClusterViolations(t.Context(), &cloudv1.ListClusterViolationsRequest{TenantId: tenant, ClusterId: id, SortBy: by, SortOrder: order})
		return err
	}
	require.NoError(t, violations(0, 0))
	require.NoError(t, violations(cloudv1.ViolationSortBy_VIOLATION_SORT_BY_NAMESPACE, cloudv1.SortOrder_SORT_ORDER_ASC))
	require.Equal(t, connect.CodeUnimplemented, connect.CodeOf(violations(cloudv1.ViolationSortBy_VIOLATION_SORT_BY_SEVERITY, 0)))
	require.Equal(t, connect.CodeUnimplemented, connect.CodeOf(violations(cloudv1.ViolationSortBy_VIOLATION_SORT_BY_ENGINE, 0)))
	require.Equal(t, connect.CodeUnimplemented, connect.CodeOf(violations(0, cloudv1.SortOrder_SORT_ORDER_DESC)))

	events := func(by cloudv1.ViolationEventSortBy, order cloudv1.SortOrder) error {
		_, err := w.svc.ListClusterViolationEvents(t.Context(), &cloudv1.ListClusterViolationEventsRequest{TenantId: tenant, ClusterId: id, SortBy: by, SortOrder: order})
		return err
	}
	require.NoError(t, events(0, 0))
	require.NoError(t, events(cloudv1.ViolationEventSortBy_VIOLATION_EVENT_SORT_BY_TIME, cloudv1.SortOrder_SORT_ORDER_DESC), "newest first is the default")
	require.Equal(t, connect.CodeUnimplemented, connect.CodeOf(events(cloudv1.ViolationEventSortBy_VIOLATION_EVENT_SORT_BY_TIME, cloudv1.SortOrder_SORT_ORDER_ASC)))
	require.Equal(t, connect.CodeUnimplemented, connect.CodeOf(events(cloudv1.ViolationEventSortBy_VIOLATION_EVENT_SORT_BY_COUNT, 0)))

	// The sort of another tenant's cluster is not told: it is not found first.
	_, err := w.svc.ListNodeGroupNodes(t.Context(), &cloudv1.ListNodeGroupNodesRequest{TenantId: "other", ClusterId: id, NodeGroup: "batch", SortBy: cloudv1.NodeSortBy_NODE_SORT_BY_AGE})
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}

func TestGetNodeResourcesIsUnimplementedUntilTheLiveReaderExists(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	for name, tt := range map[string]struct {
		req  *cloudv1.GetNodeResourcesRequest
		want connect.Code
	}{
		"own cluster":     {&cloudv1.GetNodeResourcesRequest{TenantId: worldTenant, ClusterId: w.row.ID}, connect.CodeUnimplemented},
		"foreign tenant":  {&cloudv1.GetNodeResourcesRequest{TenantId: "other", ClusterId: w.row.ID}, connect.CodeNotFound},
		"not a cluster":   {&cloudv1.GetNodeResourcesRequest{TenantId: worldTenant, ClusterId: "prod"}, connect.CodeInvalidArgument},
		"no tenant":       {&cloudv1.GetNodeResourcesRequest{ClusterId: w.row.ID}, connect.CodeInvalidArgument},
		"unknown cluster": {&cloudv1.GetNodeResourcesRequest{TenantId: worldTenant, ClusterId: "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"}, connect.CodeNotFound},
	} {
		res, err := w.svc.GetNodeResources(t.Context(), tt.req)
		require.Nil(t, res, name)
		require.Equal(t, tt.want, connect.CodeOf(err), name)
	}
}

// The page_token of the node list takes the longer live tokens; the stored
// list does not understand them and says to restart.
func TestNodePageTokenLength(t *testing.T) {
	req := func(token string) *cloudv1.ListNodeGroupNodesRequest {
		return &cloudv1.ListNodeGroupNodesRequest{TenantId: worldTenant, ClusterId: "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33", NodeGroup: "batch", PageToken: token}
	}
	require.NoError(t, service.Validate(req("live:"+strings.Repeat("a", 32)+":100")))
	require.NoError(t, service.Validate(req(strings.Repeat("1", 512))))
	require.Error(t, service.Validate(req(strings.Repeat("1", 513))))

	w := newNodesWorld(t)
	w.putNodes(t, nil, groupOf("batch", 3))
	r := req("live:0123456789abcdef0123456789abcdef:1")
	r.ClusterId = w.row.ID
	_, err := w.svc.ListNodeGroupNodes(t.Context(), r)
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	require.Contains(t, err.Error(), "restart the list")
}

// Wire compatibility: the numbers and types of what existed do not move, and
// the new fields are numbers that were free.
func TestWireCompatibility(t *testing.T) {
	type field struct {
		name   protoreflect.Name
		number protoreflect.FieldNumber
	}
	old := map[proto.Message][]field{
		&cloudv1.ListNodeGroupNodesRequest{}:          {{"tenant_id", 1}, {"cluster_id", 2}, {"node_group", 3}, {"page_size", 4}, {"page_token", 5}, {"filter", 6}, {"name_contains", 7}},
		&cloudv1.ListNodeGroupNodesResponse{}:         {{"nodes", 1}, {"next_page_token", 2}, {"total", 3}, {"collected_at", 4}, {"complete", 5}, {"notices", 6}},
		&cloudv1.ListClusterViolationsRequest{}:       {{"tenant_id", 1}, {"cluster_id", 2}, {"namespace", 3}, {"engine", 4}, {"page_size", 5}, {"page_token", 6}},
		&cloudv1.ListClusterViolationsResponse{}:      {{"violations", 1}, {"next_page_token", 2}, {"summary", 3}},
		&cloudv1.ListClusterViolationEventsRequest{}:  {{"tenant_id", 1}, {"cluster_id", 2}, {"namespace", 3}, {"engine", 4}, {"page_size", 5}, {"page_token", 6}},
		&cloudv1.ListClusterViolationEventsResponse{}: {{"events", 1}, {"next_page_token", 2}, {"summary", 3}, {"retention_notice", 4}},
		&cloudv1.ListClusterDeprecationsRequest{}:     {{"tenant_id", 1}, {"cluster_id", 2}, {"page_size", 3}, {"page_token", 4}, {"only_removed", 5}},
		&cloudv1.ListClusterDeprecationsResponse{}:    {{"findings", 1}, {"next_page_token", 2}, {"target_version", 3}, {"scanned_at", 4}, {"total", 5}, {"removed", 6}, {"truncated", 7}, {"error", 8}},
		&cloudv1.ListClusterRecommendationsRequest{}:  {{"tenant_id", 1}, {"cluster_id", 2}, {"namespace", 3}, {"page_size", 4}, {"page_token", 5}, {"only_changes", 6}},
		&cloudv1.ListClusterRecommendationsResponse{}: {{"recommendations", 1}, {"next_page_token", 2}, {"summary", 3}, {"total", 4}},
		&cloudv1.NodeGroupNode{}:                      {{"name", 1}, {"provider_id", 15}},
	}
	for msg, fields := range old {
		fd := msg.ProtoReflect().Descriptor().Fields()
		for _, f := range fields {
			got := fd.ByName(f.name)
			require.NotNil(t, got, "%s.%s", msg.ProtoReflect().Descriptor().Name(), f.name)
			require.Equal(t, f.number, got.Number(), "%s.%s", msg.ProtoReflect().Descriptor().Name(), f.name)
		}
	}
	require.EqualValues(t, 1, cloudv1.SortOrder_SORT_ORDER_ASC)
	require.EqualValues(t, 2, cloudv1.SortOrder_SORT_ORDER_DESC)
}

// An old client's bytes (no sort fields) are a request for the default sort,
// and a new client's sort fields reach an old server as unknown fields, which
// protobuf skips: the old fields decode the same.
func TestOldAndNewMessagesDecodeEachOther(t *testing.T) {
	var oldBytes []byte
	oldBytes = protowire.AppendTag(oldBytes, 1, protowire.BytesType)
	oldBytes = protowire.AppendString(oldBytes, worldTenant)
	oldBytes = protowire.AppendTag(oldBytes, 2, protowire.BytesType)
	oldBytes = protowire.AppendString(oldBytes, "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33")
	oldBytes = protowire.AppendTag(oldBytes, 3, protowire.VarintType)
	oldBytes = protowire.AppendVarint(oldBytes, 20)
	oldBytes = protowire.AppendTag(oldBytes, 4, protowire.BytesType)
	oldBytes = protowire.AppendString(oldBytes, "7")
	oldBytes = protowire.AppendTag(oldBytes, 5, protowire.VarintType)
	oldBytes = protowire.AppendVarint(oldBytes, 1)

	got := &cloudv1.ListClusterDeprecationsRequest{}
	require.NoError(t, proto.Unmarshal(oldBytes, got))
	require.Equal(t, cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_UNSPECIFIED, got.GetSortBy())
	require.Equal(t, cloudv1.SortOrder_SORT_ORDER_UNSPECIFIED, got.GetSortOrder())
	require.EqualValues(t, 20, got.GetPageSize())
	require.Equal(t, "7", got.GetPageToken())
	require.True(t, got.GetOnlyRemoved())
	require.NoError(t, service.Validate(got))
	require.Equal(t, oldBytes, mustMarshal(t, got), "an old request round-trips byte for byte")

	// New client, old server: the old fields' bytes are what an old message
	// would have had, plus fields 6 and 7 it does not know.
	req := proto.CloneOf(got)
	req.SortBy, req.SortOrder = cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_KIND, cloudv1.SortOrder_SORT_ORDER_DESC
	newBytes := mustMarshal(t, req)
	require.Equal(t, oldBytes, newBytes[:len(oldBytes)])
	rest := newBytes[len(oldBytes):]
	for len(rest) > 0 {
		num, typ, n := protowire.ConsumeTag(rest)
		require.GreaterOrEqual(t, int(num), 6, "only the new fields follow")
		rest = rest[n:]
		n = protowire.ConsumeFieldValue(num, typ, rest)
		require.Positive(t, n)
		rest = rest[n:]
	}
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	require.NoError(t, err)
	return b
}

func TestUnavailableDetailRoundTrips(t *testing.T) {
	// The detail of an UNAVAILABLE live read is a message of the API.
	d := &cloudv1.LiveUnavailableDetail{Reason: cloudv1.LiveUnavailableReason_LIVE_UNAVAILABLE_REASON_CREDENTIALS_REJECTED, Message: "the platform cannot read this cluster"}
	got := &cloudv1.LiveUnavailableDetail{}
	require.NoError(t, proto.Unmarshal(mustMarshal(t, d), got))
	require.True(t, proto.Equal(d, got))
}
