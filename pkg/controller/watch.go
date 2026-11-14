package controller

import (
	"context"
	"maps"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// batchWindow is how long the events of an owner's provider objects are
// collected before the owner is reconciled: a network or cluster being
// built has dozens of objects changing within seconds, and each pass reads
// all of them, so one pass for the lot instead of one per event.
var batchWindow = time.Second

// providerObjectChanged lets through the provider-object events an owner's
// pass can see: creates, deletes, and updates that change the object's
// generation (spec), labels (the owner lookup reads them), annotations
// (external name, paused, Crossplane's create markers), finalizers,
// deletion or anything in status (conditions and status.atProvider: the
// views read Ready and Synced, the cluster reads atProvider fields such as
// the EKS endpoint and OIDC issuer, Helm Release state and revision).
// Everything else is dropped: an informer resync (the same object again)
// and writes that move only resourceVersion or managedFields.
//
// A non-unstructured object is let through: the rule compares status as
// the cache holds provider objects, unstructured.
func providerObjectChanged() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		oldObj, okOld := e.ObjectOld.(*unstructured.Unstructured)
		newObj, okNew := e.ObjectNew.(*unstructured.Unstructured)
		if !okOld || !okNew {
			return true
		}
		return metadataChanged(oldObj, newObj) ||
			!equality.Semantic.DeepEqual(oldObj.Object["status"], newObj.Object["status"])
	}}
}

// metadataChanged reports whether what a pass reads of metadata moved.
func metadataChanged(oldObj, newObj client.Object) bool {
	return oldObj.GetGeneration() != newObj.GetGeneration() ||
		oldObj.GetDeletionTimestamp().IsZero() != newObj.GetDeletionTimestamp().IsZero() ||
		!maps.Equal(oldObj.GetLabels(), newObj.GetLabels()) ||
		!maps.Equal(oldObj.GetAnnotations(), newObj.GetAnnotations()) ||
		!slices.Equal(oldObj.GetFinalizers(), newObj.GetFinalizers())
}

// batchedOwner enqueues the requests that owner maps an event's objects to
// (the old and the new object of an update, as
// handler.EnqueueRequestsFromMapFunc does) after window, not at once. The
// queue holds one entry per request: every event of the owner's objects
// within the window joins it, and the owner is reconciled once.
func batchedOwner(owner handler.MapFunc, window time.Duration) handler.EventHandler {
	add := func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request], objs ...client.Object) {
		for _, obj := range objs {
			for _, r := range owner(ctx, obj) {
				q.AddAfter(r, window)
			}
		}
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			add(ctx, q, e.Object)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			add(ctx, q, e.ObjectOld, e.ObjectNew)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			add(ctx, q, e.Object)
		},
		GenericFunc: func(ctx context.Context, e event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			add(ctx, q, e.Object)
		},
	}
}
