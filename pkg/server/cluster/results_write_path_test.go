package cluster

import (
	"errors"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/stretchr/testify/require"
	awss3 "github.com/upbound/provider-aws/v2/apis/namespaced/s3/v1beta1"
	"google.golang.org/protobuf/encoding/protojson"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile/resultsink"
	"github.com/yindia/cluster-vending/pkg/results"
)

// writtenWorld is a results world whose scan is written by the controller's
// sink (the write path) into the same fake S3 the server reads from.
func writtenWorld(t *testing.T, bucketReady bool) (*resultsWorld, *resultsink.Sink) {
	t.Helper()
	w := newResultsWorld(t)
	deps := w.svc.deps
	deps.ResultsS3 = func(a awscloud.Access, region string) (results.S3API, error) { return w.s3, nil }
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	w.bucket = crossplaneaws.ResultsBucketName(prefix, "444455556666", "us-east-1", w.row.ID)
	b := &awss3.Bucket{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: crossplaneaws.ResultsBucketObjectName(prefix)}}
	if bucketReady {
		b.SetConditions(xpv2.Available())
	} else {
		b.SetConditions(xpv2.Creating())
	}
	require.NoError(t, w.kube.Create(t.Context(), b))
	return w, resultsink.New(deps, nil)
}

// What the controller writes, the server reads: the list pages come from S3
// through the tenant role, however many findings there are.
func TestWhatTheScannerWritesTheServerReads(t *testing.T) {
	w, sink := writtenWorld(t, true)
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	scan := findings(900, 40)

	staged, notice := sink.Stage(t.Context(), worldTenant, w.row.ID, results.Deprecations, scan, at,
		model.ResultRef{Total: 900, Removed: 40, CollectedAt: at})
	require.Empty(t, notice)
	require.NotNil(t, staged)
	require.NoError(t, staged.Commit(t.Context()))

	req := &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, PageSize: 500}
	var names []string
	for range 5 {
		res, err := w.svc.ListClusterDeprecations(t.Context(), req)
		require.NoError(t, err)
		require.EqualValues(t, 900, res.GetTotal())
		require.Empty(t, res.GetError())
		for _, f := range res.GetFindings() {
			names = append(names, f.GetName())
		}
		if res.GetNextPageToken() == "" {
			break
		}
		req.PageToken = res.GetNextPageToken()
	}
	require.Len(t, names, 900)
	require.Equal(t, "f899", names[899])
}

// When S3 cannot be written, no summary is recorded and the server answers
// from the Cluster object, with the findings the scanner kept there.
func TestWhenTheWriteFailsTheServerReadsTheClusterObject(t *testing.T) {
	w, _ := writtenWorld(t, true)
	deps := w.svc.deps
	deps.ResultsS3 = func(awscloud.Access, string) (results.S3API, error) { return nil, errors.New("S3 is down") }
	scan := findings(3, 1)
	raw, err := protojson.Marshal(scan)
	require.NoError(t, err)
	require.NoError(t, w.store.Clusters().SetDeprecations(t.Context(), worldTenant, w.row.ID, raw)) // the scanner records it first

	staged, notice := resultsink.New(deps, nil).Stage(t.Context(), worldTenant, w.row.ID, results.Deprecations, scan, time.Now(), model.ResultRef{})

	require.Nil(t, staged)
	require.NotEmpty(t, notice)
	res, err := w.svc.ListClusterDeprecations(t.Context(), &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	require.NoError(t, err)
	require.Len(t, res.GetFindings(), 3)
	require.Empty(t, res.GetError())
}
