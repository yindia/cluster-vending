package controller

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

// Reads that need not be fresh go to the manager's informer cache (Kube)
// instead of the API server (Direct): the objects a pass looks at but
// does not write (the tenant's namespace, an owner's parent, the
// releases). The object a pass writes, and anything it checks before
// letting something go (the dependents of an owner being deleted), is
// still read at the API server: a stale copy there means a conflict or a
// wrong decision, not just a later one.

// cached is the reader of the informer cache; Direct when there is none.
func (d Deps) cached() client.Reader {
	if d.Kube == nil {
		return d.Direct
	}
	return d.Kube
}

// getCached reads key into obj from the cache, and from the API server
// when the cache does not have it: not cached at all (the cache refuses
// kinds it holds no informer for, ReaderFailOnMissingInformer), or not
// (yet) there: an object created a moment ago, or one outside the
// cache's label selector.
func (d Deps) getCached(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	err := d.cached().Get(ctx, key, obj)
	if err == nil || !missingFromCache(err) {
		return err
	}
	return d.Direct.Get(ctx, key, obj)
}

// getAdopted reads the parent object key (a cluster's network, a
// network's account) into obj: from the cache when the copy there is
// adopted (it has its row ID), else from the API server, so a parent
// adopted a moment ago is not taken for one that is not.
func (d Deps) getAdopted(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	err := d.cached().Get(ctx, key, obj)
	switch {
	case err == nil && obj.GetLabels()[cloudstore.LabelID] != "":
		return nil
	case err != nil && !missingFromCache(err):
		return err
	}
	return d.Direct.Get(ctx, key, obj)
}

// listCached lists from the cache, and from the API server when the cache
// holds no informer for the kind.
func (d Deps) listCached(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := d.cached().List(ctx, list, opts...)
	if err == nil || !notCached(err) {
		return err
	}
	return d.Direct.List(ctx, list, opts...)
}

func missingFromCache(err error) bool { return apierrors.IsNotFound(err) || notCached(err) }

func notCached(err error) bool {
	var refused *cache.ErrResourceNotCached
	return errors.As(err, &refused)
}

// namespaceGVK is how the cache holds namespaces: unstructured
// (management.CachedObjects, kube.StartInformers). A typed read of one
// is refused.
var namespaceGVK = corev1.SchemeGroupVersion.WithKind("Namespace")

// tenantOf is the tenant whose namespace obj is in; "" when the namespace
// is no tenant's. A bootstrapped tenant's namespace is in the cache (the
// account applies it with the platform's labels); a newer one is read at
// the API server.
func tenantOf(ctx context.Context, d Deps, obj client.Object) (string, error) {
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(namespaceGVK)
	if err := d.getCached(ctx, client.ObjectKey{Name: obj.GetNamespace()}, ns); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	return ns.GetLabels()[cloudstore.LabelTenant], nil
}
