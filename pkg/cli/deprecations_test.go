package cli

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

const deprecationsCluster = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"

// scanService answers ListClusterDeprecations from findings, two a page
// unless the request asks otherwise.
type scanService struct {
	cloudv1connect.ClusterServiceClient
	findings []*cloudv1.DeprecationFinding
	scanned  bool
	failure  string
	err      error
	requests []*cloudv1.ListClusterDeprecationsRequest
}

func (s *scanService) ListClusterDeprecations(_ context.Context, req *cloudv1.ListClusterDeprecationsRequest) (*cloudv1.ListClusterDeprecationsResponse, error) {
	s.requests = append(s.requests, req)
	if s.err != nil {
		return nil, s.err
	}
	res := &cloudv1.ListClusterDeprecationsResponse{Error: s.failure}
	if !s.scanned {
		return res, nil
	}
	res.TargetVersion, res.ScannedAt = "1.37", timestamppb.New(time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC))
	res.Total, res.Removed, res.Truncated = int32(len(s.findings)), 1, len(s.findings) > 2
	start, _ := strconv.Atoi(req.GetPageToken())
	size := 2
	if req.GetPageSize() > 0 {
		size = int(req.GetPageSize())
	}
	end := min(start+size, len(s.findings))
	res.Findings = s.findings[start:end]
	if end < len(s.findings) {
		res.NextPageToken = strconv.Itoa(end)
	}
	return res, nil
}

func scanFindings() []*cloudv1.DeprecationFinding {
	return []*cloudv1.DeprecationFinding{
		{Namespace: "web", Kind: "Ingress", Name: "site", ApiVersion: "extensions/v1beta1", RemovedIn: "v1.22.0",
			ReplacementApi: "networking.k8s.io/v1", Removed: true, Source: cloudv1.DeprecationSource_DEPRECATION_SOURCE_MANIFEST},
		{Kind: "VolumeAttributesClass", Name: "fast", ApiVersion: "storage.k8s.io/v1beta1",
			Source: cloudv1.DeprecationSource_DEPRECATION_SOURCE_HELM_RELEASE, HelmRelease: "storage/classes"},
		{Namespace: "jobs", Kind: "CronJob", Name: "nightly", ApiVersion: "batch/v1beta1"},
	}
}

func TestClusterDeprecationsTable(t *testing.T) {
	svc := &scanService{findings: scanFindings(), scanned: true}
	var out bytes.Buffer

	err := Run(t.Context(), ClusterDeprecations, Clients{Cluster: svc},
		Options{Tenant: "acme", ID: deprecationsCluster, OnlyRemoved: true}, &out, OutputTable)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{
		"Kubernetes 1.37, scanned 2026-11-13T04:00:00Z: 3 found, 1 removed (the first 200 are kept)",
		"NAMESPACE  KIND                   NAME     API VERSION             STATE       REMOVED IN  REPLACEMENT           SOURCE",
		"web        Ingress                site     extensions/v1beta1      removed     v1.22.0     networking.k8s.io/v1  manifest",
		"-          VolumeAttributesClass  fast     storage.k8s.io/v1beta1  deprecated  -           -                     helm storage/classes",
		"jobs       CronJob                nightly  batch/v1beta1           deprecated  -           -                     manifest",
		"",
	}
	if got := out.String(); got != strings.Join(want, "\n") {
		t.Fatalf("table:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	// Every page was read, each asking for removed only.
	if len(svc.requests) != 2 || svc.requests[1].GetPageToken() != "2" || !svc.requests[0].GetOnlyRemoved() ||
		svc.requests[0].GetTenantId() != "acme" || svc.requests[0].GetClusterId() != deprecationsCluster {
		t.Errorf("requests = %v", svc.requests)
	}
}

func TestClusterDeprecationsTableStates(t *testing.T) {
	tests := []struct {
		name string
		svc  *scanService
		want string
	}{
		{"never scanned", &scanService{}, "The cluster was not scanned yet (the controller scans a running cluster every six hours).\n"},
		{"the scan failed", &scanService{failure: "kubeconfig Secret not found"},
			"The last scan failed: kubeconfig Secret not found\nThe cluster was not scanned yet (the controller scans a running cluster every six hours).\n"},
		{"nothing found", &scanService{scanned: true}, "Kubernetes 1.37, scanned 2026-11-13T04:00:00Z: 0 found, 1 removed\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := Run(t.Context(), ClusterDeprecations, Clients{Cluster: tt.svc}, Options{Tenant: "acme", ID: deprecationsCluster}, &out, OutputTable)
			if err != nil || out.String() != tt.want {
				t.Fatalf("got %q, %v; want %q", out.String(), err, tt.want)
			}
		})
	}
}

func TestClusterDeprecationsAsData(t *testing.T) {
	svc := &scanService{findings: scanFindings(), scanned: true}
	var out bytes.Buffer

	// One page was asked for: it is returned with the token of the next.
	err := Run(t.Context(), ClusterDeprecations, Clients{Cluster: svc},
		Options{Tenant: "acme", ID: deprecationsCluster, PageSize: 1}, &out, OutputJSON)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{`"name": "site"`, `"nextPageToken": "1"`, `"targetVersion": "1.37"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %s:\n%s", want, out.String())
		}
	}
	if len(svc.requests) != 1 || strings.Contains(out.String(), "fast") {
		t.Errorf("more than the page asked for: %d requests", len(svc.requests))
	}
}

func TestClusterDeprecationsFailures(t *testing.T) {
	svc := &scanService{err: errors.New("unavailable")}
	tests := []struct {
		name string
		o    Options
		want string
	}{
		{"no tenant", Options{ID: deprecationsCluster}, "tenant"},
		{"not a cluster id", Options{Tenant: "acme", ID: "prod-eks"}, "invalid request"},
		{"the API fails", Options{Tenant: "acme", ID: deprecationsCluster}, "unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ClusterDeprecations(t.Context(), Clients{Cluster: svc}, tt.o)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error with %q", err, tt.want)
			}
		})
	}
}
