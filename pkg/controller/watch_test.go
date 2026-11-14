package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// providerObject is a provider object as the cache holds it.
func providerObject() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ec2.aws.m.upbound.io/v1beta1", "kind": "VPC",
		"metadata": map[string]any{
			"name": "vpc", "namespace": "tenant-a", "generation": int64(1), "resourceVersion": "10",
			"labels":      map[string]any{"cloud.cluster-vending.io/network": "n1"},
			"annotations": map[string]any{},
		},
		"spec": map[string]any{"forProvider": map[string]any{"region": "us-west-2"}},
		"status": map[string]any{
			"atProvider": map[string]any{"id": "vpc-1"},
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "False", "reason": "Creating", "lastTransitionTime": "2026-11-15T10:00:00Z"},
				map[string]any{"type": "Synced", "status": "True", "reason": "ReconcileSuccess", "lastTransitionTime": "2026-11-15T10:00:00Z"},
			},
		},
	}}
	return u
}

func TestProviderObjectChanged(t *testing.T) {
	cases := []struct {
		name   string
		change func(u *unstructured.Unstructured)
		wakes  bool
	}{
		{"resync: the same object", func(*unstructured.Unstructured) {}, false},
		{"only the resource version", func(u *unstructured.Unstructured) { u.SetResourceVersion("11") }, false},
		{"only managed fields", func(u *unstructured.Unstructured) {
			u.SetResourceVersion("11")
			u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "provider-aws-ec2", Operation: metav1.ManagedFieldsOperationUpdate}})
		}, false},
		{"Ready flips", func(u *unstructured.Unstructured) {
			conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
			conds[0].(map[string]any)["status"] = "True"
			conds[0].(map[string]any)["reason"] = "Available"
			_ = unstructured.SetNestedSlice(u.Object, conds, "status", "conditions")
		}, true},
		{"Synced message changes", func(u *unstructured.Unstructured) {
			conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
			conds[1].(map[string]any)["status"] = "False"
			conds[1].(map[string]any)["message"] = "DependencyViolation"
			_ = unstructured.SetNestedSlice(u.Object, conds, "status", "conditions")
		}, true},
		{"atProvider changes", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "https://oidc.eks", "status", "atProvider", "identity")
		}, true},
		{"external name is set", func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{"crossplane.io/external-name": "vpc-0abc"})
		}, true},
		{"paused", func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{"crossplane.io/paused": "true"})
		}, true},
		{"owner label changes", func(u *unstructured.Unstructured) {
			u.SetLabels(map[string]string{"cloud.cluster-vending.io/network": "n2"})
		}, true},
		{"spec changes (generation)", func(u *unstructured.Unstructured) { u.SetGeneration(2) }, true},
		{"deletion requested", func(u *unstructured.Unstructured) {
			now := metav1.Now()
			u.SetDeletionTimestamp(&now)
		}, true},
		{"finalizer removed", func(u *unstructured.Unstructured) {
			u.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := providerObject()
			obj := old.DeepCopy()
			c.change(obj)
			if got := providerObjectChanged().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: obj}); got != c.wakes {
				t.Fatalf("wakes = %v, want %v", got, c.wakes)
			}
		})
	}
	p := providerObjectChanged()
	if !p.Create(event.CreateEvent{Object: providerObject()}) || !p.Delete(event.DeleteEvent{Object: providerObject()}) {
		t.Fatal("a created or deleted provider object must wake its owner")
	}
}

// recordingQueue records how requests are added.
type recordingQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]
	now   []reconcile.Request
	after map[reconcile.Request]time.Duration
}

func (q *recordingQueue) Add(r reconcile.Request) { q.now = append(q.now, r) }
func (q *recordingQueue) AddAfter(r reconcile.Request, d time.Duration) {
	if q.after == nil {
		q.after = map[reconcile.Request]time.Duration{}
	}
	q.after[r] = d
}

func TestOwnerEventsAreBatched(t *testing.T) {
	owner := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "prod"}}
	mapped := func(context.Context, client.Object) []reconcile.Request { return []reconcile.Request{owner} }
	h := batchedOwner(mapped, time.Second)
	q := &recordingQueue{}
	ctx := context.Background()
	h.Create(ctx, event.CreateEvent{Object: providerObject()}, q)
	h.Update(ctx, event.UpdateEvent{ObjectOld: providerObject(), ObjectNew: providerObject()}, q)
	h.Delete(ctx, event.DeleteEvent{Object: providerObject()}, q)
	if len(q.now) != 0 {
		t.Fatalf("added at once: %v; want every event delayed by the batch window", q.now)
	}
	if q.after[owner] != time.Second {
		t.Fatalf("delays = %v, want %v for the owner", q.after, time.Second)
	}
}

// TestBatchedEventsAreOnePass shows the queue coalescing: many children of
// one owner changing within the window make one request, not one each.
func TestBatchedEventsAreOnePass(t *testing.T) {
	owner := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "prod"}}
	mapped := func(context.Context, client.Object) []reconcile.Request { return []reconcile.Request{owner} }
	window := 50 * time.Millisecond
	h := batchedOwner(mapped, window)
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	for range 20 {
		h.Create(context.Background(), event.CreateEvent{Object: providerObject()}, q)
	}
	deadline := time.Now().Add(5 * time.Second)
	for q.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if q.Len() != 1 {
		t.Fatalf("queued = %d, want 1 request for 20 events of one owner", q.Len())
	}
}
