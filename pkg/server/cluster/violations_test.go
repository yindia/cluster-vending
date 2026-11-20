package cluster

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudapi "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/policy"
)

func recordPolicy(t *testing.T, w world, p *cloudv1.ClusterPolicySnapshot) {
	t.Helper()
	raw, err := policy.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	c := &cloudapi.Cluster{}
	if err = w.storeKube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: w.row.Name}, c); err != nil {
		t.Fatal(err)
	}
	c.Status.PolicySnapshot = string(raw)
	if err = w.storeKube.Status().Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
}
func TestPolicySnapshotListsAndScope(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	req := &cloudv1.ListClusterViolationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, PageSize: 1}
	absent, err := w.svc.ListClusterViolations(t.Context(), req)
	if err != nil || !absent.Summary.Incomplete || absent.Summary.CollectedAt != nil {
		t.Fatalf("absent %v %v", absent, err)
	}
	p := &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamppb.Now()}, Violations: []*cloudv1.ClusterViolation{{Id: "a", Engine: "kyverno", Resource: &cloudv1.PolicyObjectReference{Namespace: "apps"}}, {Id: "b", Engine: "gatekeeper", Resource: &cloudv1.PolicyObjectReference{Namespace: "other"}}}}
	recordPolicy(t, w, p)
	first, err := w.svc.ListClusterViolations(t.Context(), req)
	if err != nil || len(first.Violations) != 1 || first.NextPageToken == "" {
		t.Fatalf("first %v %v", first, err)
	}
	req.PageToken = first.NextPageToken
	second, err := w.svc.ListClusterViolations(t.Context(), req)
	if err != nil || second.Violations[0].Id != "b" || second.NextPageToken != "" {
		t.Fatalf("second %v %v", second, err)
	}
	for _, token := range []string{"-1", "bad", strings.Split(first.NextPageToken, ":")[0] + ":-1", strings.Split(first.NextPageToken, ":")[0] + ":999999999999999999999999"} {
		req.PageToken = token
		if _, err = w.svc.ListClusterViolations(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("token %s: %v", token, err)
		}
	}
	req.PageToken = first.NextPageToken
	req.Engine = "kyverno"
	if _, err = w.svc.ListClusterViolations(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal("cross filter token accepted", err)
	}
	req.Engine = ""
	req.Namespace = "apps"
	if _, err = w.svc.ListClusterViolations(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal("cross namespace token accepted", err)
	}
	req.Namespace = ""
	req.TenantId = "other"
	if _, err = w.svc.ListClusterViolations(t.Context(), req); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatal("cross tenant", err)
	}
	req.TenantId = worldTenant
	p.Summary.CollectedAt = timestamppb.New(time.Now().Add(time.Second))
	recordPolicy(t, w, p)
	if _, err = w.svc.ListClusterViolations(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal("stale token accepted", err)
	}
	req.PageToken = ""
	req.Namespace = "other"
	got, err := w.svc.ListClusterViolations(t.Context(), req)
	if err != nil || len(got.Violations) != 1 || got.Violations[0].Id != "b" {
		t.Fatalf("filter %v %v", got, err)
	}
}
func TestPolicyEventExpirationAndStaleSnapshot(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	clock := time.Now()
	oldNow := now
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = oldNow })
	p := &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamppb.New(clock.Add(-20 * time.Minute))}, Events: []*cloudv1.ClusterViolationEvent{{Id: "old", LastObservedAt: timestamppb.New(clock.Add(-2 * time.Hour))}, {Id: "a", LastObservedAt: timestamppb.New(clock.Add(-59 * time.Minute))}, {Id: "b", LastObservedAt: timestamppb.New(clock)}}}
	recordPolicy(t, w, p)
	req := &cloudv1.ListClusterViolationEventsRequest{TenantId: worldTenant, ClusterId: w.row.ID, PageSize: 1}
	got, err := w.svc.ListClusterViolationEvents(t.Context(), req)
	if err != nil || got.Events[0].Id != "a" || !got.Summary.Stale || got.Summary.Incomplete || got.RetentionNotice == "" {
		t.Fatalf("events %v %v", got, err)
	}
	req.PageToken = got.NextPageToken
	clock = clock.Add(2 * time.Minute)
	if _, err = w.svc.ListClusterViolationEvents(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal("TTL shifted page token", err)
	}
	req.PageToken = ""
	req.TenantId = "other"
	if _, err = w.svc.ListClusterViolationEvents(t.Context(), req); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatal("cross tenant events", err)
	}
}
func TestPolicyPageScopeAndCap(t *testing.T) {
	_, end, token, err := policyPage([]byte("snapshot"), []string{"tenant", "cluster"}, "", 9999, 500)
	if err != nil || end != 200 {
		t.Fatal(end, err)
	}
	for _, scope := range [][]string{{"other", "cluster"}, {"tenant", "other"}} {
		if _, _, _, err = policyPage([]byte("snapshot"), scope, token, 50, 500); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatal("cross scope token accepted", err)
		}
	}
}

func TestPolicyPageScopeNulCollision(t *testing.T) {
	_, _, token, err := policyPage([]byte("snapshot"), []string{"t", "c", "violations", "a\x00b", ""}, "", 1, 5)
	if err != nil || token == "" {
		t.Fatal(token, err)
	}
	// Same joined bytes, different field boundaries.
	if _, _, _, err = policyPage([]byte("snapshot"), []string{"t", "c", "violations", "a", "b\x00"}, token, 1, 5); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal("colliding scope accepted", err)
	}
	if _, _, _, err = policyPage([]byte("snapshot"), []string{"t", "c", "violations", "a\x00b", ""}, token, 1, 5); err != nil {
		t.Fatal(err)
	}
}

func TestPolicySummaryStaleIsDistinctFromIncomplete(t *testing.T) {
	at := time.Now()
	old := timestamppb.New(at.Add(-3 * policy.Interval))
	snap := func(collected *timestamppb.Timestamp, rows int) *cloudv1.ClusterPolicySnapshot {
		p := &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: collected}}
		for range rows {
			p.Violations = append(p.Violations, &cloudv1.ClusterViolation{Id: "x"})
		}
		return p
	}
	cases := []struct {
		name  string
		p     *cloudv1.ClusterPolicySnapshot
		stale bool
	}{
		{"fresh", snap(timestamppb.New(at), 1), false},
		{"old", snap(old, 1), true},
		{"unknown time with rows", snap(nil, 1), true},
		{"unknown time empty", snap(nil, 0), false},
	}
	for _, c := range cases {
		policySummary(c.p, at)
		if c.p.Summary.Stale != c.stale || c.p.Summary.Incomplete {
			t.Errorf("%s: stale=%v incomplete=%v", c.name, c.p.Summary.Stale, c.p.Summary.Incomplete)
		}
		if c.stale != (len(c.p.Summary.Notices) == 1) {
			t.Errorf("%s: notices %v", c.name, c.p.Summary.Notices)
		}
	}
	full := snap(old, 1)
	for range 32 {
		full.Summary.Notices = append(full.Summary.Notices, "n")
	}
	policySummary(full, at)
	if !full.Summary.Stale || len(full.Summary.Notices) != 32 {
		t.Fatal("cap", full.Summary.Stale, len(full.Summary.Notices))
	}
}
