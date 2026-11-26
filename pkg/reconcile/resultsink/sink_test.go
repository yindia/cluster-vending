package resultsink_test

import (
	"errors"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/stretchr/testify/require"
	awss3 "github.com/upbound/provider-aws/v2/apis/namespaced/s3/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/reconcile/resultsink"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/results/resultstest"
)

const (
	tenant    = "acme"
	namespace = "tenant-acme"
	clusterID = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"
	region    = "us-east-1"
	accountID = "444455556666"
)

var now = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

type world struct {
	k       client.Client
	store   repository.Store
	s3      *resultstest.S3
	bucket  string
	prefix  string
	sink    *resultsink.Sink
	deps    reconcile.Deps
	account *model.CloudAccount
	asked   []awscloud.Access
}

// newWorld is a running cluster "prod" of an account, with a fake S3. The
// cluster's Bucket managed resource is created by bucket(ready).
func newWorld(t *testing.T, imported bool) *world {
	t.Helper()
	k := storetest.NewClient(t)
	store := cloudstore.New(k)
	ctx := t.Context()
	require.NoError(t, store.Tenants().Create(ctx, &model.Tenant{ID: tenant}))
	account := &model.CloudAccount{TenantID: tenant, Provider: model.ProviderAWS, AccountID: accountID,
		ProviderConfigName: "aws-0a1b2c3d", Namespace: namespace, Identity: "arn:aws:iam::" + accountID + ":role/provisioner"}
	require.NoError(t, store.Tenants().PutCloudAccount(ctx, account))
	obj := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "prod", Labels: map[string]string{
		cloudstore.LabelTenant: tenant, cloudstore.LabelID: clusterID, cloudstore.LabelAccountID: account.ID}},
		Spec: cloudv1alpha1.ClusterSpec{Region: region}}
	require.NoError(t, k.Create(ctx, obj))
	if imported {
		row, err := store.Clusters().Get(ctx, tenant, clusterID)
		require.NoError(t, err)
		row.Imported = true
		require.NoError(t, store.Clusters().Update(ctx, row))
	}
	w := &world{k: k, store: store, account: account, prefix: crossplaneaws.ClusterPrefix(tenant, clusterID, "prod")}
	w.bucket = crossplaneaws.ResultsBucketName(w.prefix, accountID, region, clusterID)
	w.s3 = resultstest.New(w.bucket)
	w.deps = reconcile.Deps{Store: store, Kube: k, KubeReader: k,
		ResultsS3: func(a awscloud.Access, r string) (results.S3API, error) {
			w.asked = append(w.asked, a)
			require.Equal(t, region, r)
			return w.s3, nil
		}}
	w.sink = newSink(w)
	return w
}

func newSink(w *world) *resultsink.Sink { return resultsink.New(w.deps, nil) }

// bucket is the cluster's Bucket managed resource, Ready or not.
func (w *world) bucketObject(t *testing.T, ready bool) {
	t.Helper()
	b := &awss3.Bucket{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: crossplaneaws.ResultsBucketObjectName(w.prefix)}}
	if ready {
		b.SetConditions(xpv2.Available())
	} else {
		b.SetConditions(xpv2.Creating())
	}
	require.NoError(t, w.k.Create(t.Context(), b))
}

func (w *world) refOf(t *testing.T, kind results.Kind) *model.ResultRef {
	t.Helper()
	row, err := w.store.Clusters().Get(t.Context(), tenant, clusterID)
	require.NoError(t, err)
	return row.ResultRef(string(kind))
}

func scan(n int) *cloudv1.ClusterDeprecations {
	d := &cloudv1.ClusterDeprecations{TargetVersion: "1.37", Total: int32(n), Removed: 1}
	for range n {
		d.Findings = append(d.Findings, &cloudv1.DeprecationFinding{Namespace: "web", Kind: "Ingress", Name: "site", Removed: true})
	}
	return d
}

func TestStageWritesTheResultAndCommitRecordsTheSummary(t *testing.T) {
	w := newWorld(t, false)
	w.bucketObject(t, true)

	staged, notice := w.sink.Stage(t.Context(), tenant, clusterID, results.Deprecations, scan(3), now,
		model.ResultRef{Total: 3, Removed: 1, CollectedAt: now})

	require.Empty(t, notice)
	require.NotNil(t, staged)
	_, etag, ok := w.s3.Object(w.bucket, results.Deprecations.LatestKey())
	require.True(t, ok, "latest/deprecations.json is written")
	require.Contains(t, w.s3.Keys(w.bucket), results.Deprecations.HistoryKey(now))
	require.Nil(t, w.refOf(t, results.Deprecations), "nothing is recorded before Commit")
	require.NoError(t, staged.Commit(t.Context()))
	ref := w.refOf(t, results.Deprecations)
	require.NotNil(t, ref)
	require.Equal(t, model.ResultRef{Kind: "deprecations", Bucket: w.bucket, Key: "latest/deprecations.json", ETag: etag,
		CollectedAt: now, Total: 3, Removed: 1}, *ref)
	// The tenant role of the account, nothing else.
	require.Len(t, w.asked, 1)
	require.Equal(t, w.account.Identity, w.asked[0].RoleARN)
}

func TestStageFallsBackWithANoticeWhenTheBucketCannotTakeIt(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testing.T, *world)
		notice string
	}{
		{"no bucket yet", func(*testing.T, *world) {}, "not created yet"},
		{"bucket not ready", func(t *testing.T, w *world) { w.bucketObject(t, false) }, "not ready yet"},
		{"S3 fails", func(t *testing.T, w *world) {
			w.bucketObject(t, true)
			w.s3.Fail = func(op, _, _ string) error { return errors.New("boom") }
		}, "writing to the results bucket failed"},
		{"no access", func(t *testing.T, w *world) {
			w.bucketObject(t, true)
			w.deps.ResultsS3 = func(awscloud.Access, string) (results.S3API, error) { return nil, errors.New("no role") }
			w.sink = resultsink.New(w.deps, nil)
		}, "could not be reached"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, false)
			tt.setup(t, w)

			staged, notice := w.sink.Stage(t.Context(), tenant, clusterID, results.Deprecations, scan(1), now, model.ResultRef{})

			require.Nil(t, staged)
			require.Contains(t, notice, tt.notice)
			require.Nil(t, w.refOf(t, results.Deprecations), "no summary without the data in the bucket")
		})
	}
}

func TestStageKeepsLatestWhenTheHistoryWriteFails(t *testing.T) {
	w := newWorld(t, false)
	w.bucketObject(t, true)
	staged, _ := w.sink.Stage(t.Context(), tenant, clusterID, results.Deprecations, scan(1), now, model.ResultRef{})
	require.NotNil(t, staged)
	w.s3.Fail = func(_, _, key string) error {
		if key != results.Deprecations.LatestKey() {
			return errors.New("history denied")
		}
		return nil
	}

	again, notice := w.sink.Stage(t.Context(), tenant, clusterID, results.Deprecations, scan(9), now.Add(time.Hour), model.ResultRef{})

	require.Nil(t, again)
	require.NotEmpty(t, notice)
	body, etag, _ := w.s3.Object(w.bucket, results.Deprecations.LatestKey())
	require.Equal(t, staged.Ref().ETag, etag, "latest stays what it was")
	require.NotEmpty(t, body)
}

func TestAnImportedClusterWithoutABucketIsSilent(t *testing.T) {
	w := newWorld(t, true)
	row, err := w.store.Clusters().Get(t.Context(), tenant, clusterID)
	require.NoError(t, err)
	require.True(t, row.Imported)
	staged, notice := w.sink.Stage(t.Context(), tenant, clusterID, results.Deprecations, scan(1), now, model.ResultRef{})
	require.Nil(t, staged)
	require.Empty(t, notice)
}

func TestNoS3MeansNothingWrittenAndNoNotice(t *testing.T) {
	w := newWorld(t, false)
	w.bucketObject(t, true)
	w.deps.ResultsS3 = nil
	staged, notice := resultsink.New(w.deps, nil).Stage(t.Context(), tenant, clusterID, results.Deprecations, scan(1), now, model.ResultRef{})
	require.Nil(t, staged)
	require.Empty(t, notice)
	require.Empty(t, w.s3.Keys(w.bucket))
}

// failures counts ResultWriteFailed by kind (reconcile.Metrics).
type failures struct{ kinds []string }

func (*failures) ClusterProvisioned(time.Duration)     {}
func (*failures) UpgradeFinished(time.Duration)        {}
func (*failures) ComponentReady(string, time.Duration) {}
func (f *failures) ResultWriteFailed(kind string)      { f.kinds = append(f.kinds, kind) }

func TestAFailedWriteIsCountedByKind(t *testing.T) {
	w := newWorld(t, false)
	w.bucketObject(t, true)
	m := &failures{}
	w.deps.Metrics = m
	w.sink = resultsink.New(w.deps, nil)
	w.s3.Fail = func(op, _, _ string) error { return errors.New("boom") }

	_, notice := w.sink.Stage(t.Context(), tenant, clusterID, results.Deprecations, scan(1), now, model.ResultRef{})

	require.NotEmpty(t, notice)
	require.Equal(t, []string{"deprecations"}, m.kinds)
}
