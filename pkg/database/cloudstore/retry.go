package cloudstore

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

// AnnotationRetry is a request to recover what failed on a cluster
// (model.Retry as JSON): ClusterService.RetryCluster writes it, the
// controller acts on it once and removes it. A patch of the one
// annotation: the row (its etag, its spec) is not written.
const AnnotationRetry = group + "retry"

func retryOf(obj client.Object) *model.Retry {
	return model.ParseRetry(obj.GetAnnotations()[AnnotationRetry])
}

// RequestRetry implements repository.Clusters.
func (r clusters) RequestRetry(ctx context.Context, tenantID, id string, retry model.Retry) error {
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		return translate(err)
	}
	return r.writeRetry(ctx, obj, retryOf(obj).Merge(retry))
}

// ClearRetry implements repository.Clusters.
func (r clusters) ClearRetry(ctx context.Context, tenantID, id string, at time.Time) error {
	obj, err := r.find(ctx, tenantID, id)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return translate(err)
	}
	if current := retryOf(obj); current == nil || !current.At.Equal(at) {
		return nil // none, or one asked after the pass that acted
	}
	return r.writeRetry(ctx, obj, nil)
}

func (r clusters) writeRetry(ctx context.Context, obj client.Object, next *model.Retry) error {
	value := ""
	if next != nil {
		value = next.Annotation()
	}
	if value == obj.GetAnnotations()[AnnotationRetry] {
		return nil
	}
	before := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	annotate(obj, map[string]string{AnnotationRetry: value})
	return translate(r.c.Patch(ctx, obj, before))
}
