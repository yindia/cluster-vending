//go:build e2e

package e2e

import (
	"fmt"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// scenarioScanners asserts the scan results are reachable through the API:
// policy violations, deprecated APIs and recommendations. A call that fails
// is a failure; a scan that has not run yet is reported, not failed (the
// scanners run on their own schedule, and recommendations need metrics).
func (e *env) scenarioScanners(t *testing.T) {
	need(t, e.st.clusterID != "", "no cluster (scenario 3)")
	id := e.st.clusterID
	c, err := e.getCluster(id)
	if err != nil {
		t.Fatal(err)
	}
	need(t, isRunning(c), "the cluster is not RUNNING")

	// The cheap scans are waited for; their results are in the results bucket.
	var violations *cloudv1.ListClusterViolationsResponse
	var deprecations *cloudv1.ListClusterDeprecationsResponse
	err = e.waitFor(t, "violation and deprecation scans", e.cfg.ScanTimeout, func() (bool, string, error) {
		var err error
		if violations, err = rpc(e, e.api.Cluster.ListClusterViolations, &cloudv1.ListClusterViolationsRequest{TenantId: e.cfg.Tenant, ClusterId: id, PageSize: 50}); err != nil {
			return false, "", fmt.Errorf("ListClusterViolations: %w", err)
		}
		if deprecations, err = rpc(e, e.api.Cluster.ListClusterDeprecations, &cloudv1.ListClusterDeprecationsRequest{TenantId: e.cfg.Tenant, ClusterId: id, PageSize: 50}); err != nil {
			return false, "", fmt.Errorf("ListClusterDeprecations: %w", err)
		}
		got := violations.GetSummary().GetCollectedAt().IsValid() && deprecations.GetTargetVersion() != ""
		return got, fmt.Sprintf("violations collected=%v, deprecations target=%q total=%d", violations.GetSummary().GetCollectedAt().IsValid(), deprecations.GetTargetVersion(), deprecations.GetTotal()), nil
	})
	if err != nil {
		// Reachable but not collected: say so; an RPC error was returned above as a failure.
		if violations == nil || deprecations == nil {
			t.Fatal(err)
		}
		t.Logf("not collected within %s: %v", e.cfg.ScanTimeout, err)
	}
	if violations != nil {
		s := violations.GetSummary()
		t.Logf("violations: %d listed, collected %v, incomplete=%v stale=%v notices=%v", len(violations.GetViolations()), s.GetCollectedAt().IsValid(), s.GetIncomplete(), s.GetStale(), s.GetNotices())
	}
	if deprecations != nil {
		t.Logf("deprecations: %d findings of %d for %s, truncated=%v", len(deprecations.GetFindings()), deprecations.GetTotal(), deprecations.GetTargetVersion(), deprecations.GetTruncated())
		if deprecations.GetTruncated() {
			t.Error("the deprecation findings of a fresh cluster are truncated")
		}
	}

	if _, err := rpc(e, e.api.Cluster.ListClusterViolationEvents, &cloudv1.ListClusterViolationEventsRequest{TenantId: e.cfg.Tenant, ClusterId: id, PageSize: 50}); err != nil {
		t.Errorf("ListClusterViolationEvents: %v", err)
	}
	rec, err := rpc(e, e.api.Cluster.ListClusterRecommendations, &cloudv1.ListClusterRecommendationsRequest{TenantId: e.cfg.Tenant, ClusterId: id, PageSize: 50})
	if err != nil {
		t.Errorf("ListClusterRecommendations: %v", err)
	} else {
		s := rec.GetSummary()
		t.Logf("recommendations: %d of %d, sampled %v metrics=%v error=%q message=%q", len(rec.GetRecommendations()), rec.GetTotal(), s.GetSampledAt().IsValid(), s.GetMetricsAvailable(), s.GetError(), s.GetMessage())
	}
	if _, err := rpc(e, e.api.Cluster.ListClusterEvents, &cloudv1.ListClusterEventsRequest{TenantId: e.cfg.Tenant, ClusterId: id, PageSize: 50}); err != nil {
		t.Errorf("ListClusterEvents: %v", err)
	}
	if _, err := rpc(e, e.api.Cluster.ListClusterNodeGroupManifests, &cloudv1.ListClusterNodeGroupManifestsRequest{TenantId: e.cfg.Tenant, ClusterId: id}); err != nil {
		t.Errorf("ListClusterNodeGroupManifests: %v", err)
	}
}
