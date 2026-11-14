package cloudstore_test

import (
	"errors"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// An account is deleted only without networks, and through the
// controller: its finalizer stays, so the controller removes the
// account's ProviderConfig before the account goes.
func TestDeleteCloudAccount(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	obj := &cloudv1alpha1.CloudAccount{}
	key := client.ObjectKey{Namespace: namespace, Name: "aws-444455556666"}
	if err := w.k.Get(ctx, key, obj); err != nil {
		t.Fatal(err)
	}
	obj.Finalizers = append(obj.Finalizers, cloudv1alpha1.CloudAccountFinalizer)
	if err := w.k.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}

	err := w.s.Tenants().DeleteCloudAccount(ctx, "acme", w.account)
	if !errors.Is(err, repository.ErrInUse) || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("delete under a network: %v, want in use naming the network", err)
	}

	if err := w.s.Networks().Delete(ctx, "acme", w.network.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Tenants().DeleteCloudAccount(ctx, "acme", w.account); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := w.k.Get(ctx, key, obj); err != nil {
		t.Fatalf("the account went at once (%v): the controller's finalizer must hold it", err)
	}
	if obj.DeletionTimestamp.IsZero() || len(obj.Finalizers) == 0 {
		t.Errorf("deleting = %v, finalizers %v", obj.DeletionTimestamp, obj.Finalizers)
	}
	if err := w.s.Tenants().DeleteCloudAccount(ctx, "acme", "00000000-0000-0000-0000-000000000000"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("unknown account: %v, want not found", err)
	}
}
