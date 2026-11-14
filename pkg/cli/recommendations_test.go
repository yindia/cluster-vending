package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

// recommendationService answers ListClusterRecommendations, one a page.
type recommendationService struct {
	cloudv1connect.ClusterServiceClient
	rows     []*cloudv1.ResourceRecommendation
	summary  *cloudv1.ClusterRecommendations
	err      error
	requests []*cloudv1.ListClusterRecommendationsRequest
}

func (s *recommendationService) ListClusterRecommendations(_ context.Context, req *cloudv1.ListClusterRecommendationsRequest) (*cloudv1.ListClusterRecommendationsResponse, error) {
	s.requests = append(s.requests, req)
	if s.err != nil {
		return nil, s.err
	}
	res := &cloudv1.ListClusterRecommendationsResponse{Summary: s.summary, Total: int32(len(s.rows))}
	start := 0
	if req.GetPageToken() != "" {
		start = 1
	}
	if start < len(s.rows) {
		res.Recommendations = s.rows[start : start+1]
		if start+1 < len(s.rows) {
			res.NextPageToken = "1"
		}
	}
	return res, nil
}

const mib = int64(1) << 20

func recommendationRows() []*cloudv1.ResourceRecommendation {
	return []*cloudv1.ResourceRecommendation{
		{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Container: "app",
			CurrentRequests:     &cloudv1.ResourceAmounts{CpuMillis: 1000, MemoryBytes: 1024 * mib},
			CurrentLimits:       &cloudv1.ResourceAmounts{MemoryBytes: 2048 * mib},
			RecommendedRequests: &cloudv1.ResourceAmounts{CpuMillis: 120, MemoryBytes: 230 * mib},
			RecommendedLimits:   &cloudv1.ResourceAmounts{MemoryBytes: 276 * mib},
			CpuPercent:          -88, MemoryPercent: -77,
			Source: cloudv1.RecommendationSource_RECOMMENDATION_SOURCE_USAGE_ESTIMATE, DataHours: 50},
		{Namespace: "ops", WorkloadKind: "DaemonSet", WorkloadName: "agent", Container: "agent",
			Source: cloudv1.RecommendationSource_RECOMMENDATION_SOURCE_NO_DATA},
	}
}

func TestClusterRecommendationsTable(t *testing.T) {
	svc := &recommendationService{rows: recommendationRows(), summary: &cloudv1.ClusterRecommendations{
		SampledAt: timestamppb.New(time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)), Workloads: 2, Containers: 2, WindowDays: 7,
		MetricsAvailable: true, Message: "recording usage"}}
	var out bytes.Buffer

	err := Run(t.Context(), ClusterRecommendations, Clients{Cluster: svc},
		Options{Tenant: "acme", ID: deprecationsCluster, Namespace: "shop", OnlyChanges: true}, &out, OutputTable)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Sampled 2026-11-14T08:00:00Z: 2 containers of 2 workloads, 7 days kept; metrics-server: yes; VPA: no",
		"recording usage",
		"NAMESPACE", "CPU REQ", "MEM REQ", "MEM LIMIT", "SOURCE", "DATA",
		"1000m -> 120m (-88%)", "1Gi -> 230Mi (-77%)", "2Gi -> 276Mi", "usage-estimate", "2d",
		"-", "no-data",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if len(svc.requests) != 2 || svc.requests[0].GetNamespace() != "shop" || !svc.requests[0].GetOnlyChanges() {
		t.Errorf("requests = %v", svc.requests)
	}
}

func TestClusterRecommendationsNeverSampled(t *testing.T) {
	var out bytes.Buffer

	err := Run(t.Context(), ClusterRecommendations, Clients{Cluster: &recommendationService{}},
		Options{Tenant: "acme", ID: deprecationsCluster}, &out, OutputTable)

	if err != nil || !strings.Contains(out.String(), "not sampled yet") {
		t.Fatalf("Run = %v, output %q", err, out.String())
	}
}

func TestClusterRecommendationsAsData(t *testing.T) {
	var out bytes.Buffer

	err := Run(t.Context(), ClusterRecommendations, Clients{Cluster: &recommendationService{rows: recommendationRows()}},
		Options{Tenant: "acme", ID: deprecationsCluster}, &out, OutputJSON)

	got := strings.Join(strings.Fields(out.String()), "")
	if err != nil || !strings.Contains(got, `"workloadName":"web"`) || !strings.Contains(got, `"workloadName":"agent"`) {
		t.Fatalf("Run = %v:\n%s", err, out.String())
	}
}

func TestClusterRecommendationsErrors(t *testing.T) {
	err := Run(t.Context(), ClusterRecommendations, Clients{Cluster: &recommendationService{err: errors.New("boom")}},
		Options{Tenant: "acme", ID: deprecationsCluster}, &bytes.Buffer{}, OutputTable)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Run = %v", err)
	}
	if err := Run(t.Context(), ClusterRecommendations, Clients{Cluster: &recommendationService{}},
		Options{ID: deprecationsCluster}, &bytes.Buffer{}, OutputTable); err == nil {
		t.Error("ran without --tenant")
	}
}

func TestQuantities(t *testing.T) {
	tests := []struct {
		cpu  int64
		mem  int64
		want string
	}{
		{1500, 0, "1500m"}, {0, 512 * mib, "512Mi"}, {0, 3 * 1024 * mib, "3Gi"}, {0, 1500 * 1024, "2Mi"}, {0, 100, "100B"},
	}
	for _, tt := range tests {
		got := millicores(tt.cpu)
		if tt.mem != 0 || tt.cpu == 0 {
			got = binary(tt.mem)
		}
		if got != tt.want {
			t.Errorf("cpu %d mem %d = %q, want %q", tt.cpu, tt.mem, got, tt.want)
		}
	}
}
