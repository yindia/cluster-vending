package cloudstore_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	networkreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
)

func fenceAccount(t *testing.T, w world) *cloud.CloudAccount {
	t.Helper()
	obj := &cloud.CloudAccount{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: "aws-444455556666"}, obj); err != nil {
		t.Fatal(err)
	}
	obj.Finalizers = []string{cloud.CloudAccountFinalizer}
	if err := w.k.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func fenceNetwork(t *testing.T, w world) *model.Network {
	t.Helper()
	n := &model.Network{TenantID: "acme", Name: "racing", Provider: model.ProviderAWS, Region: "us-east-1", CloudAccountID: w.account,
		Resource: model.Resource{State: "NETWORK_STATE_PROVISIONING"}}
	n.Spec = document(t, networkSpec(n))
	return n
}

func TestNetworkCreateRejectsTerminatingAccount(t *testing.T) {
	w := newWorld(t)
	account := fenceAccount(t, w)
	if err := w.k.Delete(t.Context(), account); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Networks().Create(t.Context(), fenceNetwork(t, w)); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("terminating account accepted new network", err)
	}
	obj := &cloud.Network{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: "racing"}, obj); !apierrors.IsNotFound(err) {
		t.Fatal("rejected create persisted intent", err)
	}
}

func TestNetworkAccountFenceRefusesDeletePreparedBeforeInsertion(t *testing.T) {
	w := newWorld(t)
	account := fenceAccount(t, w)
	// This is exactly the account snapshot a deletion holds after listing no
	// matching new child. Existing fixture network is unrelated to the CAS proof.
	version := account.ResourceVersion
	if err := w.s.Networks().Create(t.Context(), fenceNetwork(t, w)); err != nil {
		t.Fatal(err)
	}
	err := w.k.Delete(t.Context(), account, client.Preconditions{ResourceVersion: &version})
	if !apierrors.IsConflict(err) {
		t.Fatal("stale account deletion won after network creation", err)
	}
}

func TestNetworkCreateAndAccountDeleteCannotBothCommit(t *testing.T) {
	w := newWorld(t)
	fenceAccount(t, w)
	if err := w.s.Networks().Delete(t.Context(), "acme", w.network.ID); err != nil {
		t.Fatal(err)
	}
	deletePrepared := make(chan struct{})
	proceed := make(chan struct{})
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*cloud.CloudAccount); ok {
			close(deletePrepared)
			<-proceed
		}
		return c.Delete(ctx, obj, opts...)
	}})
	s := cloudstore.New(k)
	deleted := make(chan error, 1)
	go func() { deleted <- s.Tenants().DeleteCloudAccount(t.Context(), "acme", w.account) }()
	<-deletePrepared // account snapshot and empty dependency list already read
	row := fenceNetwork(t, w)
	err := s.Networks().Create(t.Context(), row)
	close(proceed)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; !errors.Is(err, repository.ErrConflict) {
		t.Fatal("both competing lifecycle operations committed", err)
	}
	if _, err := s.Networks().Get(t.Context(), "acme", row.ID); err != nil {
		t.Fatal("winning child not usable", err)
	}
}

func TestNetworkCreateRollsBackWhenAccountDeleteWinsDuringInsertion(t *testing.T) {
	w := newWorld(t)
	account := fenceAccount(t, w)
	var raced atomic.Bool
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*cloud.Network); ok && raced.CompareAndSwap(false, true) {
			if err := c.Delete(ctx, account); err != nil {
				return err
			}
		}
		return c.Create(ctx, obj, opts...)
	}})
	s := cloudstore.New(k)
	row := fenceNetwork(t, w)
	if err := s.Networks().Create(t.Context(), row); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("account deletion race was accepted", err)
	}
	obj := &cloud.Network{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: row.Name}, obj); !apierrors.IsNotFound(err) {
		t.Fatal("unsafe child survived account deletion", err)
	}
}

func TestPendingNetworkCannotReconcileBeforeAccountFence(t *testing.T) {
	w := newWorld(t)
	var sawPending atomic.Bool
	var s repository.Store
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		if network, ok := obj.(*cloud.Network); ok {
			_, err := s.Networks().Get(ctx, "acme", network.Labels[cloudstore.LabelID])
			if !errors.Is(err, repository.ErrConflict) {
				t.Fatal("pending child was readable as provisionable intent", err)
			}
			sawPending.Store(true)
		}
		return nil
	}})
	s = cloudstore.New(k)
	if err := s.Networks().Create(t.Context(), fenceNetwork(t, w)); err != nil || !sawPending.Load() {
		t.Fatal("pending fence not observed or did not complete", err)
	}
}

func TestPendingNetworkFenceRecoversOnlyAgainstOriginalActiveAccount(t *testing.T) {
	w := newWorld(t)
	account := fenceAccount(t, w)
	row := fenceNetwork(t, w)
	request := uuid.NewString()
	row.CreateRequestID = &request
	if err := w.s.Networks().Create(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	obj := &cloud.Network{}
	key := client.ObjectKey{Namespace: namespace, Name: row.Name}
	if err := w.k.Get(t.Context(), key, obj); err != nil {
		t.Fatal(err)
	}
	obj.Annotations["cloud.cluster-vending.io/account-fence-pending"] = "legacy-no-uid"
	if err := w.k.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if err := cloudstore.ResumeNetworkCreate(t.Context(), w.s, "acme", request); err != nil {
		t.Fatal("exact create retry could not finish fence", err)
	}
	if _, err := w.s.Networks().Get(t.Context(), "acme", row.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Get(t.Context(), key, obj); err != nil {
		t.Fatal(err)
	}
	obj.Annotations["cloud.cluster-vending.io/account-fence-pending"] = "legacy-no-uid"
	if err := w.k.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Get(t.Context(), client.ObjectKeyFromObject(account), account); err != nil {
		t.Fatal(err)
	}
	account.DeletionTimestamp = nil
	if err := w.k.Delete(t.Context(), account); err != nil {
		t.Fatal(err)
	}
	if err := cloudstore.ResumeNetworkAccountFence(t.Context(), w.s, "acme", row.ID); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("resume activated a network against deleting account", err)
	}
}

func TestNativeNetworkAdoptionRejectsDeletingAccount(t *testing.T) {
	w := newWorld(t)
	account := fenceAccount(t, w)
	if err := w.k.Delete(t.Context(), account); err != nil {
		t.Fatal(err)
	}
	row := fenceNetwork(t, w)
	obj := &cloud.Network{ObjectMeta: metav1.ObjectMeta{Name: row.Name, Namespace: namespace}}
	if err := w.k.Create(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if err := cloudstore.AdoptNetwork(t.Context(), w.s, obj, row); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("native network bypassed deletion guard", err)
	}
}

func TestNativeAdoptionStatusCrashResumesBeforeControllerRowRead(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprintf("accountDeleting=%v", deleting), func(t *testing.T) {
			w := newWorld(t)
			account := fenceAccount(t, w)
			account.UID = "original-account-uid"
			if err := w.k.Update(t.Context(), account); err != nil {
				t.Fatal(err)
			}
			obj := &cloud.Network{}
			if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: w.network.Name}, obj); err != nil {
				t.Fatal(err)
			}
			obj.Name, obj.ResourceVersion, obj.UID = "native-crash", "", ""
			obj.Labels, obj.Annotations, obj.OwnerReferences = nil, nil, nil
			obj.Status = cloud.NetworkStatus{}
			if err := w.k.Create(t.Context(), obj); err != nil {
				t.Fatal(err)
			}
			boom := errors.New("crash after native row metadata persistence")
			interrupted := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*cloud.Network); ok && sub == "status" {
					return boom
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}})
			row := fenceNetwork(t, w)
			row.Name = obj.Name
			if err := cloudstore.AdoptNetwork(t.Context(), cloudstore.New(interrupted), obj, row); !errors.Is(err, boom) {
				t.Fatal("failed to inject native adoption interruption", err)
			}
			// A fresh store models process restart; there is no request UUID and
			// the native CR already has AnnotationRow, so adoption is skipped.
			restarted := cloudstore.New(w.k)
			if _, err := restarted.Networks().Get(t.Context(), "acme", row.ID); !errors.Is(err, repository.ErrConflict) {
				t.Fatal("pure read bypassed pending fence", err)
			}
			if deleting {
				if err := w.k.Delete(t.Context(), account); err != nil {
					t.Fatal(err)
				}
			}
			rec := networkreconcile.New(reconcile.Deps{Store: restarted, Kube: w.k, KubeReader: w.k})
			err := rec.Reconcile(t.Context(), "acme", row.ID)
			if deleting {
				if !errors.Is(err, repository.ErrConflict) {
					t.Fatal("controller resumed against deleting account", err)
				}
				if _, err := restarted.Networks().Get(t.Context(), "acme", row.ID); !errors.Is(err, repository.ErrConflict) {
					t.Fatal("failed resume made native intent provisionable", err)
				}
			} else {
				// Later provider/application prerequisites may fail in this fake;
				// the actual controller entry point must still complete the fence.
				if _, readErr := restarted.Networks().Get(t.Context(), "acme", row.ID); readErr != nil {
					t.Fatalf("controller never recovered pending intent: %v (reconcile: %v)", readErr, err)
				}
			}
		})
	}
}
