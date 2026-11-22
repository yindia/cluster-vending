package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

// policyService answers the violation RPCs: two pages each.
type policyService struct {
	cloudv1connect.ClusterServiceClient
	violations []*cloudv1.ListClusterViolationsRequest
	events     []*cloudv1.ListClusterViolationEventsRequest
}

var policyCollected = timestamppb.New(time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC))

func (s *policyService) ListClusterViolations(_ context.Context, req *cloudv1.ListClusterViolationsRequest) (*cloudv1.ListClusterViolationsResponse, error) {
	s.violations = append(s.violations, req)
	v := &cloudv1.ClusterViolation{Engine: "kyverno", Severity: "high", Result: "fail", Rule: "require-labels",
		Policy:   &cloudv1.PolicyObjectReference{Kind: "ClusterPolicy", Name: "require-labels"},
		Resource: &cloudv1.PolicyObjectReference{Kind: "Pod", Namespace: "web", Name: "site-1"},
		Message:  "label team is required\non every pod"}
	res := &cloudv1.ListClusterViolationsResponse{Violations: []*cloudv1.ClusterViolation{v},
		Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: policyCollected, Stale: true, Notices: []string{"gatekeeper is not installed"}}}
	if req.GetPageToken() == "" {
		res.NextPageToken = "2"
	}
	return res, nil
}

func (s *policyService) ListClusterViolationEvents(_ context.Context, req *cloudv1.ListClusterViolationEventsRequest) (*cloudv1.ListClusterViolationEventsResponse, error) {
	s.events = append(s.events, req)
	e := &cloudv1.ClusterViolationEvent{Reason: "PolicyViolation", Count: 4, Action: "blocked",
		Resource: &cloudv1.PolicyObjectReference{Kind: "Deployment", Namespace: "web", Name: "site"},
		Policy:   &cloudv1.PolicyObjectReference{Kind: "ClusterPolicy", Name: "require-labels"}, Message: "denied"}
	return &cloudv1.ListClusterViolationEventsResponse{Events: []*cloudv1.ClusterViolationEvent{e},
		Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: policyCollected}, RetentionNotice: "kept for one hour"}, nil
}

func TestClusterViolationsTable(t *testing.T) {
	svc := &policyService{}
	var out bytes.Buffer
	err := Run(t.Context(), ClusterViolations, Clients{Cluster: svc},
		Options{Tenant: "acme", ID: nodesCluster, Namespace: "web", Engine: "kyverno", SortBy: "severity"}, &out, OutputTable)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(svc.violations) != 2 || svc.violations[1].GetPageToken() != "2" {
		t.Fatalf("requests = %v", svc.violations)
	}
	r := svc.violations[0]
	if r.GetNamespace() != "web" || r.GetEngine() != "kyverno" || r.GetSortBy() != cloudv1.ViolationSortBy_VIOLATION_SORT_BY_SEVERITY {
		t.Errorf("request = %v", r)
	}
	got := out.String()
	for _, want := range []string{"2 violations, collected 2026-11-22 03:00:00 UTC (stale)", "Notice: gatekeeper is not installed",
		"Pod/site-1", "ClusterPolicy/require-labels", "label team is required on every pod"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestClusterViolationEvents(t *testing.T) {
	svc := &policyService{}
	var out bytes.Buffer
	err := Run(t.Context(), ClusterViolations, Clients{Cluster: svc},
		Options{Tenant: "acme", ID: nodesCluster, Events: true, SortBy: "count", SortOrder: "asc"}, &out, OutputTable)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(svc.events) != 1 || len(svc.violations) != 0 ||
		svc.events[0].GetSortBy() != cloudv1.ViolationEventSortBy_VIOLATION_EVENT_SORT_BY_COUNT ||
		svc.events[0].GetSortOrder() != cloudv1.SortOrder_SORT_ORDER_ASC {
		t.Fatalf("requests: events %v, violations %v", svc.events, svc.violations)
	}
	for _, want := range []string{"1 policy events in the last hour", "Notice: kept for one hour", "PolicyViolation", "Deployment/site", "blocked"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	// A violations sort key is not an events one.
	if err := Run(t.Context(), ClusterViolations, Clients{Cluster: svc}, Options{Tenant: "acme", ID: nodesCluster, Events: true, SortBy: "severity"}, &out, OutputTable); err == nil ||
		!strings.Contains(err.Error(), "want one of time, count") {
		t.Errorf("err = %v", err)
	}
}

func TestStoredListsSendTheSort(t *testing.T) {
	dep := &scanService{findings: scanFindings(), scanned: true}
	if err := Run(t.Context(), ClusterDeprecations, Clients{Cluster: dep}, Options{Tenant: "acme", ID: nodesCluster, SortBy: "kind", SortOrder: "desc"}, &bytes.Buffer{}, OutputTable); err != nil {
		t.Fatal(err)
	}
	if r := dep.requests[0]; r.GetSortBy() != cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_KIND || r.GetSortOrder() != cloudv1.SortOrder_SORT_ORDER_DESC {
		t.Errorf("deprecations request = %v", r)
	}
	rec := &recommendationService{}
	if err := Run(t.Context(), ClusterRecommendations, Clients{Cluster: rec}, Options{Tenant: "acme", ID: nodesCluster, SortBy: "cpu-change"}, &bytes.Buffer{}, OutputTable); err != nil {
		t.Fatal(err)
	}
	if r := rec.requests[0]; r.GetSortBy() != cloudv1.RecommendationSortBy_RECOMMENDATION_SORT_BY_CPU_CHANGE || r.GetSortOrder() != cloudv1.SortOrder_SORT_ORDER_UNSPECIFIED {
		t.Errorf("recommendations request = %v", r)
	}
	if err := Run(t.Context(), ClusterRecommendations, Clients{Cluster: rec}, Options{Tenant: "acme", ID: nodesCluster, SortBy: "kind"}, &bytes.Buffer{}, OutputTable); err == nil {
		t.Error("a deprecations key was accepted for recommendations")
	}
}
