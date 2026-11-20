package controller

import (
	"context"
	"time"

	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/policy"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/stretchr/testify/require"
	awss3 "github.com/upbound/provider-aws/v2/apis/namespaced/s3/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/recommend"
	"github.com/yindia/cluster-vending/pkg/reconcile/resultsink"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/results/resultstest"
)

const scannedClusterID = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"

// withResults gives the world a fake S3 and the cluster "prod" its Bucket
// managed resource (Ready or not), as a platform-created cluster has.
func withResults(t *testing.T, w *world, bucketReady bool) (*resultstest.S3, string) {
	t.Helper()
	ctx := t.Context()
	obj := &cloudv1alpha1.Cluster{}
	w.must(t, w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "prod"}, obj))
	obj.Labels[cloudstore.LabelAccountID] = w.account.ID
	w.must(t, w.k.Update(ctx, obj))
	prefix := crossplaneaws.ClusterPrefix(tenant, scannedClusterID, "prod")
	bucket := crossplaneaws.ResultsBucketName(prefix, w.account.AccountID, "us-east-1", scannedClusterID)
	fake := resultstest.New(bucket)
	w.deps.ResultsS3 = func(a awscloud.Access, region string) (results.S3API, error) {
		if a.RoleARN != w.account.Identity || region != "us-east-1" {
			return nil, errors.New("not the tenant role of the cluster's region")
		}
		return fake, nil
	}
	b := &awss3.Bucket{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: crossplaneaws.ResultsBucketObjectName(prefix)}}
	if bucketReady {
		b.SetConditions(xpv2.Available())
	} else {
		b.SetConditions(xpv2.Creating())
	}
	w.must(t, w.k.Create(ctx, b))
	return fake, bucket
}

// With a Ready bucket the scan is written whole to S3 and the Cluster keeps
// the counts and the key.
func TestDeprecationScanIsWrittenToTheResultsBucket(t *testing.T) {
	w, r, _ := scanWorld(t)
	s3, bucket := withResults(t, &w, true)
	r.sink = resultsink.New(w.deps.Deps, nil)

	_, err := r.Reconcile(t.Context(), request("prod"))

	require.NoError(t, err)
	got := &cloudv1.ClusterDeprecations{}
	_, _, rerr := results.NewReader(s3).Latest(t.Context(), bucket, results.Deprecations, got)
	require.NoError(t, rerr)
	require.Len(t, got.Findings, 1)
	st := w.cluster(t, "prod").Status
	require.Len(t, st.Results, 1)
	ref := st.Results[0]
	require.Equal(t, cloudv1alpha1.ResultKindDeprecations, ref.Kind)
	require.Equal(t, bucket, ref.Bucket)
	require.Equal(t, "latest/deprecations.json", ref.Key)
	require.NotEmpty(t, ref.ETag)
	require.NotEmpty(t, ref.Digest)
	require.EqualValues(t, 1, ref.Total)
	require.EqualValues(t, 1, ref.Removed)
	require.NotNil(t, st.Deprecations)
	require.Empty(t, st.Deprecations.Findings, "the Cluster keeps counts only")
	require.EqualValues(t, 1, st.Deprecations.Total)
}

// A bucket that is not Ready, or an S3 that fails, never fails the scan:
// the Cluster keeps the findings as before.
func TestDeprecationScanFallsBackToTheClusterObject(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, *world, *resultstest.S3){
		"bucket not ready": nil,
		"S3 fails": func(_ *testing.T, _ *world, s3 *resultstest.S3) {
			s3.Fail = func(op, _, _ string) error { return fmt.Errorf("%s: boom", op) }
		},
	} {
		t.Run(name, func(t *testing.T) {
			w, r, _ := scanWorld(t)
			s3, _ := withResults(t, &w, setup != nil)
			if setup != nil {
				setup(t, &w, s3)
			}
			r.sink = resultsink.New(w.deps.Deps, nil)

			_, err := r.Reconcile(t.Context(), request("prod"))

			require.NoError(t, err)
			st := w.cluster(t, "prod").Status
			require.Empty(t, st.Results)
			require.NotNil(t, st.Deprecations)
			require.Len(t, st.Deprecations.Findings, 1, "the CR keeps the data")
			require.Empty(t, st.Deprecations.Error, "an S3 problem is not a failed scan")
		})
	}
}

// The sample's recommendations are written to S3 with the summary, and the
// Cluster keeps the summary and the key.
func TestRecommendationsAreWrittenToTheResultsBucket(t *testing.T) {
	w, r, _ := recommendationWorld(t)
	s3, bucket := withResults(t, &w, true)
	r.sink = resultsink.New(w.deps.Deps, nil)

	_, err := r.Reconcile(t.Context(), request("prod"))

	require.NoError(t, err)
	set := &cloudv1.ListClusterRecommendationsResponse{}
	_, _, rerr := results.NewReader(s3).Latest(t.Context(), bucket, results.Recommendations, set)
	require.NoError(t, rerr)
	require.EqualValues(t, 1, set.GetSummary().GetContainers())
	st := w.cluster(t, "prod").Status
	require.Len(t, st.Results, 1)
	require.Equal(t, cloudv1alpha1.ResultKindRecommendations, st.Results[0].Kind)
	require.EqualValues(t, set.GetTotal(), st.Results[0].Total)
	require.NotNil(t, st.Recommendations)
	require.NotContains(t, st.Recommendations.Message, "results bucket")
}

// A bucket that is not Ready leaves the summary with a notice and the
// sample unharmed.
func TestRecommendationsFallBackWithANotice(t *testing.T) {
	w, r, _ := recommendationWorld(t)
	withResults(t, &w, false)
	r.sink = resultsink.New(w.deps.Deps, nil)

	_, err := r.Reconcile(t.Context(), request("prod"))

	require.NoError(t, err)
	st := w.cluster(t, "prod").Status
	require.Empty(t, st.Results)
	require.NotNil(t, st.Recommendations)
	require.Empty(t, st.Recommendations.Error)
	require.Contains(t, st.Recommendations.Message, "not ready yet")
}

func policyOf(n int) func(context.Context, []byte, time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
	return func(_ context.Context, _ []byte, now time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
		p := &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamppb.New(now)}}
		for i := range n {
			p.Violations = append(p.Violations, &cloudv1.ClusterViolation{Id: fmt.Sprintf("v%05d", i), Result: "fail", ObservedAt: timestamppb.New(now)})
		}
		return p, nil
	}
}

// Past the Cluster object's cap (200 rows) the whole snapshot is in S3, and
// the Cluster keeps the two summaries and no snapshot.
func TestPolicySnapshotIsWrittenWholeToTheResultsBucket(t *testing.T) {
	w, _, _ := scanWorld(t)
	s3, bucket := withResults(t, &w, true)
	r := NewPolicyReconciler(w.deps)
	require.True(t, r.full, "with S3 the collection is not bounded by the Cluster object's caps")
	r.collect = policyOf(1200)

	_, err := r.Reconcile(t.Context(), request("prod"))

	require.NoError(t, err)
	violations := &cloudv1.ClusterPolicySnapshot{}
	_, _, rerr := results.NewReader(s3).Latest(t.Context(), bucket, results.Violations, violations)
	require.NoError(t, rerr)
	require.Len(t, violations.Violations, 1200)
	events := &cloudv1.ClusterPolicySnapshot{}
	_, _, rerr = results.NewReader(s3).Latest(t.Context(), bucket, results.ViolationEvents, events)
	require.NoError(t, rerr)
	st := w.cluster(t, "prod").Status
	require.Len(t, st.Results, 2)
	require.EqualValues(t, 1200, st.Results[0].Total)
	require.Equal(t, cloudv1alpha1.ResultKindViolations, st.Results[0].Kind)
	require.Empty(t, st.PolicySnapshot, "no snapshot in the Cluster once both results are in the bucket")

	// The next reconcile knows the collection time from the summary: no rescan.
	calls := 0
	r.collect = func(context.Context, []byte, time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
		calls++
		return nil, errors.New("rescanned")
	}
	_, err = r.Reconcile(t.Context(), request("prod"))
	require.NoError(t, err)
	require.Zero(t, calls)
}

// Without the bucket the Cluster keeps the bounded snapshot, as before, with
// a notice.
func TestPolicySnapshotFallsBackToTheClusterObjectWithANotice(t *testing.T) {
	w, _, _ := scanWorld(t)
	withResults(t, &w, false)
	r := NewPolicyReconciler(w.deps)
	r.collect = policyOf(300)

	_, err := r.Reconcile(t.Context(), request("prod"))

	require.NoError(t, err)
	st := w.cluster(t, "prod").Status
	require.Empty(t, st.Results)
	stored := policy.Decode([]byte(st.PolicySnapshot))
	require.Len(t, stored.Violations, policy.MaxRows)
	require.True(t, stored.Summary.Truncated)
	require.Contains(t, stored.Summary.Notices, "scan results are kept in the cluster object: the results bucket is not ready yet")
}

// A recommendation set changes with every sample, so its history is kept once
// an hour: the latest object is still written on every change.
func TestRecommendationHistoryIsWrittenOncePerHour(t *testing.T) {
	w, r, _ := recommendationWorld(t)
	s3, bucket := withResults(t, &w, true)
	r.sink = resultsink.New(w.deps.Deps, nil)
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	histories := func() int {
		n := 0
		for _, k := range s3.Keys(bucket) {
			if strings.HasPrefix(k, "history/recommendations/") {
				n++
			}
		}
		return n
	}
	publish := func(containers int32, after time.Duration) {
		r.publish(t.Context(), tenant, scannedClusterID, &cloudv1.ClusterRecommendations{Containers: containers}, recommend.History{}, at.Add(after))
	}
	latest := func() int32 {
		set := &cloudv1.ListClusterRecommendationsResponse{}
		_, _, err := results.NewReader(s3).Latest(t.Context(), bucket, results.Recommendations, set)
		require.NoError(t, err)
		return set.GetSummary().GetContainers()
	}

	publish(1, 0)
	require.Equal(t, 1, histories())
	publish(2, 5*time.Minute)
	require.Equal(t, 1, histories(), "no second history object within the hour")
	require.EqualValues(t, 2, latest(), "latest still follows every change")
	publish(2, 10*time.Minute)
	puts := len(s3.Puts)
	publish(2, 15*time.Minute)
	require.Len(t, s3.Puts, puts, "an unchanged set writes nothing")
	publish(3, 61*time.Minute)
	require.Equal(t, 2, histories(), "an hour after the first: history again")
	require.EqualValues(t, 3, latest())
	// Another cluster has its own hour.
	require.True(t, r.history.due("other-cluster", at))
}
