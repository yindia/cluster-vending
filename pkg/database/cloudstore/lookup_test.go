package cloudstore_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
)

// A row is listed by its ID once; later reads of it get the object where
// it was found, at the API server (as fresh as the list was). An object
// that is gone, or that another row's object replaced under the same
// name, is not taken for the row.
func TestRowIsListedOnceThenGot(t *testing.T) {
	var lists, gets atomic.Int64
	k := interceptor.NewClient(storetest.NewClient(t).(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*cloudv1alpha1.NetworkList); ok {
				lists.Add(1)
			}
			return c.List(ctx, list, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*cloudv1alpha1.Network); ok {
				gets.Add(1)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	s := cloudstore.New(k)
	ctx := t.Context()
	storetest.MustTenant(t, s, "acme")
	n := &model.Network{TenantID: "acme", Name: "shared", Provider: "aws", Region: "us-east-1",
		CloudAccountID: storetest.MustAccount(t, s, "acme"), Resource: model.Resource{State: "NETWORK_STATE_PROVISIONING"}}
	n.Spec = document(t, networkSpec(n))
	if err := s.Networks().Create(ctx, n); err != nil {
		t.Fatal(err)
	}

	lists.Store(0)
	gets.Store(0)
	for range 3 {
		got, err := s.Networks().Get(ctx, "acme", n.ID)
		if err != nil || got.ID != n.ID {
			t.Fatalf("Get = %+v, %v", got, err)
		}
	}
	if lists.Load() != 1 || gets.Load() != 2 {
		t.Errorf("three reads of a row: %d lists and %d gets, want 1 and 2", lists.Load(), gets.Load())
	}

	// Another row's object under the same name: not the row.
	obj := &cloudv1alpha1.Network{}
	if err := k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "shared"}, obj); err != nil {
		t.Fatal(err)
	}
	obj.Labels[cloudstore.LabelID] = "another-row"
	if err := k.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Networks().Get(ctx, "acme", n.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("the row after its object became another row's: %v, want ErrNotFound", err)
	}

	// Gone: not found, and found again by its ID once it is back.
	if err := k.Delete(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Networks().Get(ctx, "acme", "another-row"); err == nil {
		// first lookup of "another-row" lists, finds nothing
		t.Error("a deleted row was found")
	}
	back := obj.DeepCopy()
	back.ObjectMeta = metav1.ObjectMeta{Namespace: namespace, Name: "moved", Labels: obj.Labels, Annotations: obj.Annotations}
	if err := k.Create(ctx, back); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Networks().Get(ctx, "acme", "another-row"); err != nil || got.Name != "moved" {
		t.Errorf("the row under its new name: %+v, %v", got, err)
	}
}
