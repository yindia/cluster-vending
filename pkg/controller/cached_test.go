package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
)

// asTheCacheHoldsThem is a reader that refuses what the manager's cache
// refuses (ReaderFailOnMissingInformer): a typed read of a kind it holds
// unstructured only (namespaces, the provider kinds), and Secrets
// (never cached).
func asTheCacheHoldsThem(t *testing.T, c client.WithWatch) client.WithWatch {
	t.Helper()
	refused := func(obj runtime.Object) error {
		gvk, err := apiutil.GVKForObject(obj, c.Scheme())
		if err != nil {
			return err
		}
		gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")
		_, isUnstructured := obj.(runtime.Unstructured)
		switch {
		case gvk.Kind == "Secret":
		case gvk.Group == cloudv1alpha1.GroupVersion.Group: // the platform's kinds: cached typed (For)
			return nil
		case isUnstructured:
			return nil
		}
		return &cache.ErrResourceNotCached{GVK: gvk}
	}
	return interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := refused(obj); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := refused(list); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
	})
}

// reads counts the reads a client sends, by operation and kind.
type reads struct {
	mu sync.Mutex
	n  map[string]int
}

func (r *reads) note(op string, obj runtime.Object) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kind := fmt.Sprintf("%T", obj)
	if u, ok := obj.(*unstructured.Unstructured); ok {
		kind = u.GetKind()
	}
	r.n[op+" "+kind]++
}

func (r *reads) of(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[key]
}

func (r *reads) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n = map[string]int{}
}

func countingReads(c client.WithWatch, r *reads) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			r.note("get", obj)
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			r.note("list", list)
			return c.List(ctx, list, opts...)
		},
	})
}

// A cluster created by hand is adopted with the cache holding kinds as
// the manager's does: the tenant's namespace and the network are read
// from the cache, not the API server, and every pass after that reads its
// rows with gets, not with a list of the tenant's clusters each time.
func TestAdoptionReadsTheCacheAndRowsAreGot(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.readyNetwork(t)
	// The tenant namespace as the account applies it: in the cache.
	ns := &corev1.Namespace{}
	w.must(t, w.k.Get(ctx, client.ObjectKey{Name: namespace}, ns))
	w.must(t, w.k.Create(ctx, handMadeCluster(t, "prod", "net")))

	// The controller's own reads at the API server, and the store's (the
	// store always reads there: a row is read right after it was written).
	r, rows := &reads{n: map[string]int{}}, &reads{n: map[string]int{}}
	d := Deps{Direct: countingReads(w.k.(client.WithWatch), r), Deps: platform.Deps{
		Store: cloudstore.New(countingReads(w.k.(client.WithWatch), rows)),
		Kube:  asTheCacheHoldsThem(t, w.k.(client.WithWatch))}}
	rec := NewClusterReconciler(d)

	if _, err := rec.Reconcile(ctx, request("prod")); err != nil {
		t.Fatal(err)
	}
	obj := w.cluster(t, "prod")
	if obj.Labels[cloudstore.LabelID] == "" {
		t.Fatalf("not adopted: %+v", obj.Status.Conditions)
	}
	if n := r.of("get *v1.Namespace") + r.of("get Namespace") + r.of("get *v1alpha1.Network"); n != 0 {
		t.Errorf("the controller read the namespace or the network %d times at the API server, want the cache (reads: %v)", n, r.n)
	}

	for range 3 { // applied, then settled
		if _, err := rec.Reconcile(ctx, request("prod")); err != nil {
			t.Fatal(err)
		}
	}
	rows.reset()
	if _, err := rec.Reconcile(ctx, request("prod")); err != nil {
		t.Fatal(err)
	}
	// Before: a list of the tenant's clusters for each of the pass's 7
	// lookups of its row.
	if n := rows.of("list *v1alpha1.ClusterList"); n != 0 {
		t.Errorf("a settled pass listed the tenant's clusters %d times, want gets only (reads: %v)", n, rows.n)
	}
}
