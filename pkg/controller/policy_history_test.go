package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/results/resultstest"
)

func TestPolicyPutOptions(t *testing.T) {
	require.Empty(t, policyPutOptions(true), "a due history object is the writer's default")
	require.Len(t, policyPutOptions(false), 1, "latest only")
}

// historyKeys are the keys of s3's puts under history/<kind>/.
func historyKeys(s3 *resultstest.S3, kind results.Kind) []string {
	var out []string
	for _, k := range s3.Puts {
		if strings.Contains(k, "/"+results.HistoryPrefix+string(kind)+"/") {
			out = append(out, k)
		}
	}
	return out
}

func policyPass(t *testing.T, r *PolicyReconciler, obj *cloudv1alpha1.Cluster, at time.Time, n int) {
	t.Helper()
	snap := &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamppb.New(at)},
		Violations: []*cloudv1.ClusterViolation{{Id: "v", Result: "fail", Message: string(rune('a' + n))}},
		Events:     []*cloudv1.ClusterViolationEvent{{Id: "e", Reason: "PolicyViolation", Count: int32(n)}}}
	staged, notices := r.stage(context.Background(), obj, snap, at, true)
	require.Empty(t, notices)
	for _, st := range staged {
		require.NoError(t, st.Commit(context.Background()))
	}
}

// Events never get a history object; violations get one at most an hour.
func TestPolicyPassWritesNoEventHistoryAndHourlyViolationHistory(t *testing.T) {
	w, _, _ := scanWorld(t)
	s3, _ := withResults(t, &w, true)
	r := NewPolicyReconciler(w.deps)
	obj := w.cluster(t, "prod")
	start := time.Now().UTC().Truncate(time.Second)

	for i := range 13 { // 13 passes, five minutes apart: an hour
		s3.Puts = nil
		policyPass(t, r, obj, start.Add(time.Duration(i)*5*time.Minute), i)
		require.Empty(t, historyKeys(s3, results.ViolationEvents), "pass %d", i)
		require.Contains(t, strings.Join(s3.Puts, " "), results.ViolationEvents.LatestKey(), "latest events still written")
		wantHistory := 0
		if i == 0 || i == 12 {
			wantHistory = 1
		}
		require.Len(t, historyKeys(s3, results.Violations), wantHistory, "pass %d", i)
		// latest + at most one history object for violations, latest for events.
		require.Len(t, s3.Puts, 2+wantHistory, "PUTs of pass %d", i)
	}
	for _, k := range s3.Keys(s3bucket(s3)) {
		require.NotContains(t, k, "history/violation-events/")
	}
}

func s3bucket(s *resultstest.S3) string {
	for b := range s.Buckets {
		return b
	}
	return ""
}
