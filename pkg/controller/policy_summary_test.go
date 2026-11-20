package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The policy pass records the violation counts by severity and the events of
// the last hour (PROP-02 5.2) on the Cluster status; a failed collection
// keeps them.
func TestPolicyControllerRecordsCountsAndKeepsThemOnFailure(t *testing.T) {
	w, _, _ := scanWorld(t)
	r := NewPolicyReconciler(w.deps)
	fail := false
	r.collect = func(ctx context.Context, k []byte, now time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
		if fail {
			return nil, errors.New("boom")
		}
		return &cloudv1.ClusterPolicySnapshot{
			Summary:    &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamppb.New(now)},
			Violations: []*cloudv1.ClusterViolation{{Id: "a", Severity: "high"}, {Id: "b", Severity: "high"}, {Id: "c", Severity: "low"}},
			Events: []*cloudv1.ClusterViolationEvent{
				{Id: "e1", Reason: "PolicyViolation", LastObservedAt: timestamppb.New(now.Add(-time.Minute))},
				{Id: "e2", Reason: "FailedAdmission", LastObservedAt: timestamppb.New(now.Add(-2 * time.Hour))},
			},
		}, nil
	}
	if _, err := r.Reconcile(t.Context(), request("prod")); err != nil {
		t.Fatal(err)
	}
	st := w.cluster(t, "prod").Status
	if st.Violations == nil || st.Violations.Total != 3 || len(st.Violations.BySeverity) != 2 || st.Violations.BySeverity[0].Name != "high" {
		t.Fatalf("violations = %+v", st.Violations)
	}
	if st.ViolationEvents == nil || st.ViolationEvents.Total != 1 || st.ViolationEvents.ByReason[0].Name != "PolicyViolation" {
		t.Fatalf("events = %+v", st.ViolationEvents)
	}
	if st.Violations.PolicyCollectedAt == nil {
		t.Fatal("collected time missing")
	}

	// A failed collection later keeps the counts (and their age).
	fail = true
	c := w.cluster(t, "prod")
	c.Status.PolicySnapshot = ""
	w.must(t, w.k.Status().Update(t.Context(), c))
	if _, err := r.Reconcile(t.Context(), request("prod")); err != nil {
		t.Fatal(err)
	}
	after := w.cluster(t, "prod").Status
	if after.Violations == nil || after.Violations.Total != 3 || !after.Violations.PolicyCollectedAt.Equal(st.Violations.PolicyCollectedAt) {
		t.Fatalf("counts lost or moved: %+v", after.Violations)
	}
}
