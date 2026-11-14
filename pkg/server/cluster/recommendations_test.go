package cluster

import (
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/recommend"
)

var sampleStart = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

// recorded stores two days of usage for three workloads of two
// namespaces: shop/web far above its use, shop/db close to it, ops/agent
// with no usage.
func recorded(t *testing.T, w world) {
	t.Helper()
	workloads := []recommend.Workload{
		{Namespace: "shop", Kind: "Deployment", Name: "web", Replicas: 2,
			Containers: []recommend.Container{{Name: "app", Requests: recommend.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30}}}},
		{Namespace: "shop", Kind: "StatefulSet", Name: "db", Replicas: 1,
			Containers: []recommend.Container{{Name: "pg", Requests: recommend.Resources{CPUMillis: 230, MemoryBytes: 340 << 20}}}},
		{Namespace: "ops", Kind: "DaemonSet", Name: "agent", Replicas: 3, Containers: []recommend.Container{{Name: "agent"}}},
	}
	h := recommend.History{}
	for hour := 0; hour <= 48; hour += 12 {
		h = h.Add(recommend.Snapshot{At: sampleStart.Add(time.Duration(hour) * time.Hour), Workloads: workloads, Metrics: true,
			Usage: map[recommend.Key]recommend.Resources{
				{Namespace: "shop", Kind: "Deployment", Name: "web", Container: "app"}: {CPUMillis: 100, MemoryBytes: 200 << 20},
				{Namespace: "shop", Kind: "StatefulSet", Name: "db", Container: "pg"}:  {CPUMillis: 200, MemoryBytes: 300 << 20},
			}}, nil)
	}
	raw, _, err := h.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.Clusters().SetUsageHistory(t.Context(), worldTenant, w.row.ID, raw); err != nil {
		t.Fatalf("SetUsageHistory: %v", err)
	}
	summary := `{"workloads":3,"containers":3,"windowDays":7,"metricsAvailable":true}`
	if err := w.store.Clusters().SetRecommendations(t.Context(), worldTenant, w.row.ID, []byte(summary)); err != nil {
		t.Fatalf("SetRecommendations: %v", err)
	}
}

func containers(res *cloudv1.ListClusterRecommendationsResponse) string {
	out := ""
	for _, r := range res.GetRecommendations() {
		out += fmt.Sprintf("%s/%s/%s:%s ", r.GetNamespace(), r.GetWorkloadName(), r.GetContainer(), r.GetSource())
	}
	return out
}

func TestListClusterRecommendations(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	defer func(clock func() time.Time) { now = clock }(now)
	now = func() time.Time { return sampleStart.Add(48 * time.Hour) }
	list := func(req *cloudv1.ListClusterRecommendationsRequest) *cloudv1.ListClusterRecommendationsResponse {
		t.Helper()
		req.TenantId, req.ClusterId = worldTenant, w.row.ID
		res, err := w.svc.ListClusterRecommendations(ctx, req)
		if err != nil {
			t.Fatalf("ListClusterRecommendations: %v", err)
		}
		return res
	}

	if res := list(&cloudv1.ListClusterRecommendationsRequest{}); len(res.GetRecommendations()) != 0 || res.GetSummary() != nil {
		t.Fatalf("before any sample: %v", res)
	}
	recorded(t, w)

	tests := []struct {
		name string
		req  *cloudv1.ListClusterRecommendationsRequest
		want string
		next string
	}{
		{"all, ordered", &cloudv1.ListClusterRecommendationsRequest{},
			"ops/agent/agent:RECOMMENDATION_SOURCE_NO_DATA shop/web/app:RECOMMENDATION_SOURCE_USAGE_ESTIMATE shop/db/pg:RECOMMENDATION_SOURCE_USAGE_ESTIMATE ", ""},
		{"one namespace", &cloudv1.ListClusterRecommendationsRequest{Namespace: "ops"}, "ops/agent/agent:RECOMMENDATION_SOURCE_NO_DATA ", ""},
		{"only what changes by a tenth or more", &cloudv1.ListClusterRecommendationsRequest{OnlyChanges: true},
			"shop/web/app:RECOMMENDATION_SOURCE_USAGE_ESTIMATE ", ""},
		{"a page", &cloudv1.ListClusterRecommendationsRequest{PageSize: 1}, "ops/agent/agent:RECOMMENDATION_SOURCE_NO_DATA ", "1"},
		{"the next page", &cloudv1.ListClusterRecommendationsRequest{PageSize: 1, PageToken: "1"},
			"shop/web/app:RECOMMENDATION_SOURCE_USAGE_ESTIMATE ", "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := list(tt.req)

			if got := containers(res); got != tt.want || res.GetNextPageToken() != tt.next {
				t.Fatalf("got %q next %q, want %q next %q", got, res.GetNextPageToken(), tt.want, tt.next)
			}
			if res.GetSummary().GetContainers() != 3 || !res.GetSummary().GetMetricsAvailable() {
				t.Errorf("summary = %v", res.GetSummary())
			}
		})
	}

	web := list(&cloudv1.ListClusterRecommendationsRequest{OnlyChanges: true}).GetRecommendations()[0]
	if web.GetRecommendedRequests().GetCpuMillis() != 120 || web.GetRecommendedRequests().GetMemoryBytes() != 230<<20 ||
		web.GetDifference().GetCpuMillis() != 120-1000 || web.GetCpuPercent() != -88 || web.GetDataHours() != 48 {
		t.Errorf("web = %v", web)
	}

	// The cluster's status carries the summary, not the rows.
	got, err := w.svc.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	if err != nil || got.GetCluster().GetStatus().GetRecommendations().GetContainers() != 3 {
		t.Fatalf("GetCluster status recommendations = %v, %v", got.GetCluster().GetStatus().GetRecommendations(), err)
	}
}

func TestListClusterRecommendationsRefuses(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	tests := []struct {
		name string
		req  *cloudv1.ListClusterRecommendationsRequest
		code connect.Code
	}{
		{"a bad page token", &cloudv1.ListClusterRecommendationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, PageToken: "9"},
			connect.CodeInvalidArgument},
		{"a namespace that cannot be one", &cloudv1.ListClusterRecommendationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, Namespace: "Bad_NS"},
			connect.CodeInvalidArgument},
		{"another tenant's cluster", &cloudv1.ListClusterRecommendationsRequest{TenantId: "other", ClusterId: w.row.ID},
			connect.CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := w.svc.ListClusterRecommendations(t.Context(), tt.req)

			if connect.CodeOf(err) != tt.code {
				t.Fatalf("err = %v, want %v", err, tt.code)
			}
		})
	}
}
