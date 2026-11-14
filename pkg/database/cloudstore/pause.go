package cloudstore

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

// AnnotationPaused is an operator's pause (model.Pause as JSON: who,
// since when, why): on a CloudAccount, Network or Cluster the controller
// does not act on, and on a tenant's namespace, whose new accounts,
// networks and clusters are created with it. Tenants.SetPause writes it;
// a row update never touches it.
const AnnotationPaused = group + "paused"

// pauseOf is the pause an object carries; nil for none.
func pauseOf(obj client.Object) *model.Pause {
	return model.ParsePause(obj.GetAnnotations()[AnnotationPaused])
}

// inheritPause gives a new object of a paused tenant the tenant's pause:
// what is created while a tenant is paused is paused from its first
// moment, so the controller never acts on it in between.
func (s store) inheritPause(ctx context.Context, obj client.Object) error {
	ns := &corev1.Namespace{}
	if err := s.c.Get(ctx, client.ObjectKey{Name: obj.GetNamespace()}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return translate(err)
	}
	if value := ns.Annotations[AnnotationPaused]; value != "" && obj.GetAnnotations()[AnnotationPaused] == "" {
		annotate(obj, map[string]string{AnnotationPaused: value})
	}
	return nil
}

// SetPause implements repository.Tenants.
func (r tenants) SetPause(ctx context.Context, tenantID string, target model.PauseTarget, pause *model.Pause) error {
	ns, err := r.tenantNamespace(ctx, tenantID)
	if err != nil {
		return err
	}
	scope := target.Scope()
	switch {
	case target.ClusterID != "":
		obj, err := clusters(r).find(ctx, tenantID, target.ClusterID)
		if err != nil {
			return err
		}
		return r.markPaused(ctx, obj, scope, pause, true)
	case target.NetworkID != "":
		obj, err := networks(r).find(ctx, tenantID, target.NetworkID)
		if err != nil {
			return err
		}
		return r.markPaused(ctx, obj, scope, pause, true)
	}
	// The namespace first: what is created from here on is paused.
	if err := r.markPaused(ctx, ns, scope, pause, true); err != nil {
		return err
	}
	labels := map[string]string{LabelTenant: tenantID}
	accounts, err := items[*cloudv1alpha1.CloudAccount](ctx, r.store, &cloudv1alpha1.CloudAccountList{}, ns.Name, labels)
	if err != nil {
		return err
	}
	nets, err := items[*cloudv1alpha1.Network](ctx, r.store, &cloudv1alpha1.NetworkList{}, ns.Name, labels)
	if err != nil {
		return err
	}
	all, err := items[*cloudv1alpha1.Cluster](ctx, r.store, &cloudv1alpha1.ClusterList{}, ns.Name, labels)
	if err != nil {
		return err
	}
	objects := make([]client.Object, 0, len(accounts)+len(nets)+len(all))
	for _, o := range accounts {
		objects = append(objects, o)
	}
	for _, o := range nets {
		objects = append(objects, o)
	}
	for _, o := range all {
		objects = append(objects, o)
	}
	for _, obj := range objects {
		// A tenant's resume removes its own scope only: a network or
		// cluster paused on its own stays paused.
		if err := r.markPaused(ctx, obj, scope, pause, false); err != nil {
			return err
		}
	}
	return nil
}

// markPaused records a pause of obj at scope, or with nil a resume at
// scope (model.Pause.With, Without: own is a resume of obj's own scope,
// which ends its pause). An object paused already keeps who paused it
// first, and when. It is a patch of the one annotation: the row (its etag,
// its spec) is not written, and no update of the row is lost to it.
func (s store) markPaused(ctx context.Context, obj client.Object, scope model.PauseScope, pause *model.Pause, own bool) error {
	current := pauseOf(obj)
	next := current.Without(scope, own)
	if pause != nil {
		next = current.With(scope, pause)
	}
	value := ""
	if next != nil {
		value = next.Annotation()
	}
	if value == obj.GetAnnotations()[AnnotationPaused] {
		return nil
	}
	before := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	annotate(obj, map[string]string{AnnotationPaused: value})
	err := s.c.Patch(ctx, obj, before)
	if apierrors.IsNotFound(err) {
		return nil // deleted meanwhile: nothing to pause
	}
	return translate(err)
}
