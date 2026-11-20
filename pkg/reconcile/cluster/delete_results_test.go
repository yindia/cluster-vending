package cluster

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	awss3 "github.com/upbound/provider-aws/v2/apis/namespaced/s3/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/results/resultstest"
)

// deletingWorld is a cluster whose results bucket holds objects, being
// deleted. keysAtBucketDelete are the bucket's keys the moment the Bucket
// managed resource is deleted.
type deletingWorld struct {
	world
	s3                 *resultstest.S3
	bucket             string
	keysAtBucketDelete *[]string
}

func newDeletingWorld(t *testing.T, objects ...string) deletingWorld {
	t.Helper()
	d := deletingWorld{s3: nil, keysAtBucketDelete: new([]string)}
	holder := &d
	funcs := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		_, typed := obj.(*awss3.Bucket)
		if (typed || obj.GetObjectKind().GroupVersionKind().Kind == "Bucket") && holder.s3 != nil {
			*holder.keysAtBucketDelete = holder.s3.Keys(holder.bucket)
			if len(*holder.keysAtBucketDelete) == 0 {
				*holder.keysAtBucketDelete = []string{} // emptied
			}
		}
		return c.Delete(ctx, obj, opts...)
	}}
	d.world = newWorld(t, funcs, nil, func(deps *reconcile.Deps) {
		deps.ResultsS3 = func(a awscloud.Access, region string) (results.S3API, error) {
			if a.RoleARN == "" || region != "us-east-1" {
				return nil, errors.New("not the tenant role in the cluster's region")
			}
			return holder.s3, nil
		}
	})
	d.bucket = crossplaneaws.ResultsBucketName(crossplaneaws.ClusterPrefix(worldTenant, d.row.ID, "prod"), "444455556666", "us-east-1", d.row.ID)
	d.s3 = resultstest.New(d.bucket)
	for _, key := range objects {
		d.s3.Set(d.bucket, key, []byte("{}"))
	}
	require.NoError(t, d.svc.Reconcile(t.Context(), worldTenant, d.row.ID)) // renders the bucket
	row := d.cluster(t)
	row.State = StateDeleting
	require.NoError(t, d.store.Clusters().Update(t.Context(), row))
	return d
}

func (d deletingWorld) bucketExists(t *testing.T) bool {
	t.Helper()
	err := d.kube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ResultsBucketObjectName(crossplaneaws.ClusterPrefix(worldTenant, d.row.ID, "prod"))}, &awss3.Bucket{})
	return err == nil
}

// The bucket's results are deleted first, and only then the Bucket managed
// resource; the cluster then finishes deleting.
func TestDeletionEmptiesTheResultsBucketBeforeTheBucketIsDeleted(t *testing.T) {
	d := newDeletingWorld(t, "latest/deprecations.json", "history/deprecations/2026-11-19T10:00:00Z.json", "latest/violations.json")
	require.True(t, d.bucketExists(t), "the cluster has its Bucket")

	for range 8 {
		require.NoError(t, d.svc.Reconcile(t.Context(), worldTenant, d.row.ID))
	}

	require.NotNil(t, *d.keysAtBucketDelete, "the Bucket was deleted")
	require.Empty(t, *d.keysAtBucketDelete, "the bucket was empty when its Bucket resource was deleted")
	require.Empty(t, d.s3.Keys(d.bucket))
	_, err := d.store.Clusters().Get(t.Context(), worldTenant, d.row.ID)
	require.ErrorIs(t, err, repository.ErrNotFound)
}

// An S3 that refuses holds the deletion, with the reason as the cluster's
// failure, and the Bucket resource is left alone; a later pass finishes.
func TestDeletionWaitsWhenTheBucketCannotBeEmptied(t *testing.T) {
	d := newDeletingWorld(t, "latest/deprecations.json")
	d.s3.Fail = func(op, _, _ string) error {
		if op == "ListObjectsV2" {
			return errors.New("throttled")
		}
		return nil
	}

	err := d.svc.Reconcile(t.Context(), worldTenant, d.row.ID)

	require.Error(t, err)
	require.Contains(t, err.Error(), "results bucket")
	require.True(t, d.bucketExists(t), "the Bucket is not deleted while the objects are there")
	require.Nil(t, *d.keysAtBucketDelete)
	cond := d.cluster(t)
	require.Contains(t, string(cond.Status), "empty the results bucket", "the reason is in the cluster's status")

	d.s3.Fail = nil
	for range 8 {
		require.NoError(t, d.svc.Reconcile(t.Context(), worldTenant, d.row.ID))
	}
	_, gone := d.store.Clusters().Get(t.Context(), worldTenant, d.row.ID)
	require.ErrorIs(t, gone, repository.ErrNotFound)
}

// A bucket that is not there any more is as good as empty.
func TestDeletionOfAClusterWhoseBucketIsAlreadyGone(t *testing.T) {
	d := newDeletingWorld(t)
	d.s3.Buckets = map[string]bool{} // NoSuchBucket

	for range 8 {
		require.NoError(t, d.svc.Reconcile(t.Context(), worldTenant, d.row.ID))
	}

	_, err := d.store.Clusters().Get(t.Context(), worldTenant, d.row.ID)
	require.ErrorIs(t, err, repository.ErrNotFound)
}
