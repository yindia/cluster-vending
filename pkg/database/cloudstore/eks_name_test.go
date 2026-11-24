package cloudstore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	clusterreconcile "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

func reservationPeer(t *testing.T, w world, tenant, region string) *model.Network {
	t.Helper()
	storetest.MustTenant(t, w.s, tenant)
	n := &model.Network{TenantID: tenant, Name: "shared", Provider: model.ProviderAWS, Region: region,
		CloudAccountID: storetest.MustAccount(t, w.s, tenant), Resource: model.Resource{State: "NETWORK_STATE_PROVISIONING"}}
	n.Spec = document(t, networkSpec(n))
	if err := w.s.Networks().Create(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	return n
}

func reservationCluster(t *testing.T, n *model.Network, name string) *model.Cluster {
	t.Helper()
	c := &model.Cluster{TenantID: n.TenantID, Name: name, Provider: model.ProviderAWS, Region: n.Region, NetworkID: n.ID,
		Resource: model.Resource{State: "CLUSTER_STATE_PROVISIONING"}}
	c.Spec = document(t, clusterSpec(c))
	return c
}

func TestPhysicalEKSNameConcurrentCrossTenantAllocation(t *testing.T) {
	w := newWorld(t)
	other := reservationPeer(t, w, "other", w.network.Region)
	rows := []*model.Cluster{reservationCluster(t, w.network, "payments"), reservationCluster(t, other, "payments")}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, row := range rows {
		go func() { <-start; results <- w.s.Clusters().Create(t.Context(), row) }()
	}
	close(start)
	wins := 0
	for range 2 {
		if err := <-results; err == nil {
			wins++
		} else if !errors.Is(err, repository.ErrAlreadyExists) {
			t.Fatal(err)
		} else if strings.Contains(err.Error(), "tenant-") || strings.Contains(err.Error(), "other") || strings.Contains(err.Error(), "acme") {
			t.Fatal("conflict leaked tenant identity", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d physical owners allocated", wins)
	}
	claims := &corev1.ConfigMapList{}
	if err := w.k.List(t.Context(), claims, client.InNamespace(cloudstore.EKSNameReservationNamespace)); err != nil || len(claims.Items) != 1 {
		t.Fatal(claims, err)
	}
	if claims.Items[0].Data["key"] != "aws/444455556666/us-east-1/payments" || len(claims.Items[0].OwnerReferences) != 0 {
		t.Fatal("claim lost physical scope or used an invalid cross-namespace owner", claims.Items[0])
	}
}

func TestPhysicalEKSNameAllowsOtherRegionsAndAccounts(t *testing.T) {
	w := newWorld(t)
	other := reservationPeer(t, w, "other", "us-west-2")
	for _, network := range []*model.Network{w.network, other} {
		if err := w.s.Clusters().Create(t.Context(), reservationCluster(t, network, "payments")); err != nil {
			t.Fatal(err)
		}
	}
	storetest.MustTenant(t, w.s, "third")
	account := &model.CloudAccount{TenantID: "third", Provider: model.ProviderAWS, AccountID: "999988887777"}
	if err := w.s.Tenants().PutCloudAccount(t.Context(), account); err != nil {
		t.Fatal(err)
	}
	n := &model.Network{TenantID: "third", Name: "shared", CloudAccountID: account.ID, Provider: model.ProviderAWS, Region: "us-east-1"}
	n.Spec = document(t, networkSpec(n))
	if err := w.s.Networks().Create(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Clusters().Create(t.Context(), reservationCluster(t, n, "payments")); err != nil {
		t.Fatal(err)
	}
}

func TestPhysicalEKSNameLegacyBackfillAndImportedCollision(t *testing.T) {
	w := newWorld(t)
	row := reservationCluster(t, w.network, "payments")
	row.Imported = true
	if err := w.s.Clusters().Create(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	claims := &corev1.ConfigMapList{}
	if err := w.k.List(t.Context(), claims, client.InNamespace(cloudstore.EKSNameReservationNamespace)); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Delete(t.Context(), &claims.Items[0]); err != nil {
		t.Fatal(err)
	}
	other := reservationPeer(t, w, "other", row.Region)
	if err := w.s.Clusters().Create(t.Context(), reservationCluster(t, other, row.Name)); !errors.Is(err, repository.ErrAlreadyExists) {
		t.Fatal("unclaimed legacy import was not protected", err)
	}
	before := row.ID
	if err := cloudstore.EnsureEKSNameReservation(t.Context(), w.s, row); err != nil || row.ID != before {
		t.Fatal("backfill changed identity", err)
	}
	obj := &cloud.Cluster{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: row.Name}, obj); err != nil {
		t.Fatal(err)
	}
	duplicate := obj.DeepCopy()
	duplicate.Namespace, duplicate.ResourceVersion, duplicate.UID = "tenant-other", "", ""
	duplicate.Labels = map[string]string{cloudstore.LabelID: uuid.NewString(), cloudstore.LabelTenant: "other", cloudstore.LabelNetworkID: other.ID, cloudstore.LabelAccountID: other.CloudAccountID}
	duplicate.OwnerReferences = nil
	if err := w.k.Create(t.Context(), duplicate); err != nil {
		t.Fatal(err)
	}
	// Duplicates introduced outside the reservation protocol must be caught
	// when backfilling an unclaimed identity. Established owners use direct gets;
	// a cooperating duplicate reconciler is refused by the existing claim.
	claim := &corev1.ConfigMap{}
	if err := w.k.Get(t.Context(), client.ObjectKeyFromObject(&claims.Items[0]), claim); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Delete(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if err := cloudstore.EnsureEKSNameReservation(t.Context(), w.s, row); !errors.Is(err, repository.ErrAlreadyExists) {
		t.Fatal("legacy duplicate did not fail closed", err)
	}
}

func TestPhysicalEKSNameCrashRetryRetainsAllocatedID(t *testing.T) {
	w := newWorld(t)
	var fail atomic.Bool
	fail.Store(true)
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*cloud.Cluster); ok && fail.Load() {
			return fmt.Errorf("crash after reservation")
		}
		return c.Create(ctx, obj, opts...)
	}, Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*corev1.ConfigMap); ok && fail.Load() {
			return fmt.Errorf("process died before it could release the reservation")
		}
		return c.Delete(ctx, obj, opts...)
	}})
	s := cloudstore.New(k)
	row := reservationCluster(t, w.network, "payments")
	request := uuid.NewString()
	row.CreateRequestID = &request
	if err := s.Clusters().Create(t.Context(), row); err == nil {
		t.Fatal("simulated failure ignored")
	}
	originalID := row.ID
	fail.Store(false)
	retry := reservationCluster(t, w.network, "payments")
	retry.CreateRequestID = &request
	if err := s.Clusters().Create(t.Context(), retry); err != nil || retry.ID != originalID {
		t.Fatal("exact retry failed to recover reserved identity", err)
	}
}

func TestPhysicalEKSNameReleaseWaitsForInventoryAndForeignFinalizers(t *testing.T) {
	w := newWorld(t)
	row := reservationCluster(t, w.network, "payments")
	if err := w.s.Clusters().Create(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := w.s.ManagedResources().Put(t.Context(), []model.ManagedResource{{TenantID: "acme", CloudAccountID: w.account, OwnerKind: model.OwnerCluster, OwnerID: row.ID, APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "Cluster", Namespace: namespace, Name: "physical-payments"}}); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Clusters().Delete(t.Context(), "acme", row.ID); !errors.Is(err, repository.ErrInUse) {
		t.Fatal("inventory did not retain reservation", err)
	}
	if err := w.s.ManagedResources().DeleteByOwner(t.Context(), model.OwnerCluster, row.ID); err != nil {
		t.Fatal(err)
	}
	obj := &cloud.Cluster{}
	key := client.ObjectKey{Namespace: namespace, Name: row.Name}
	if err := w.k.Get(t.Context(), key, obj); err != nil {
		t.Fatal(err)
	}
	obj.Finalizers = []string{"example.com/protection"}
	if err := w.k.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Clusters().Delete(t.Context(), "acme", row.ID); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("foreign finalizer did not retain reservation", err)
	}
	claims := &corev1.ConfigMapList{}
	if err := w.k.List(t.Context(), claims, client.InNamespace(cloudstore.EKSNameReservationNamespace)); err != nil || len(claims.Items) != 1 {
		t.Fatal("reservation released while CR remained", err)
	}
	clean := reservationCluster(t, w.network, "clean")
	if err := w.s.Clusters().Create(t.Context(), clean); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Clusters().Delete(t.Context(), "acme", clean.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Clusters().Create(t.Context(), reservationCluster(t, w.network, "clean")); err != nil {
		t.Fatal("confirmed deletion did not release physical name", err)
	}
}

func TestNativeClusterAdoptionReservesPhysicalIdentity(t *testing.T) {
	w := newWorld(t)
	row := reservationCluster(t, w.network, "payments")
	obj := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: row.Name, Namespace: namespace}}
	if err := w.k.Create(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if err := cloudstore.AdoptCluster(t.Context(), w.s, obj, row); err != nil {
		t.Fatal(err)
	}
	other := reservationPeer(t, w, "other", row.Region)
	if err := w.s.Clusters().Create(t.Context(), reservationCluster(t, other, row.Name)); !errors.Is(err, repository.ErrAlreadyExists) {
		t.Fatal("native registration did not reserve physical EKS identity", err)
	}
}

func TestNativeClusterReservationRetryUsesOriginalUID(t *testing.T) {
	w := newWorld(t)
	row := reservationCluster(t, w.network, "payments")
	obj := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: row.Name, Namespace: namespace, UID: types.UID("native-original")}}
	if err := w.k.Create(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	fail.Store(true)
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*cloud.Cluster); ok && fail.Swap(false) {
			return fmt.Errorf("crash after native reservation")
		}
		return c.Update(ctx, obj, opts...)
	}})
	s := cloudstore.New(k)
	if err := cloudstore.AdoptCluster(t.Context(), s, obj, row); err == nil {
		t.Fatal("simulated failure ignored")
	}
	originalID := row.ID
	if err := w.k.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	retry := reservationCluster(t, w.network, row.Name)
	if err := cloudstore.AdoptCluster(t.Context(), s, obj, retry); err != nil || retry.ID != originalID {
		t.Fatal("native retry did not preserve allocation ID", err)
	}
	if err := cloudstore.EnsureEKSNameReservation(t.Context(), s, retry); err != nil {
		t.Fatal(err)
	}
	// Losing/recreating the management record is not proof of cloud absence.
	if err := w.k.Delete(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	replacement := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: row.Name, Namespace: namespace, UID: types.UID("native-replacement")}}
	if err := w.k.Create(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := cloudstore.AdoptCluster(t.Context(), s, replacement, reservationCluster(t, w.network, row.Name)); !errors.Is(err, repository.ErrAlreadyExists) {
		t.Fatal("replacement incarnation stole existing reservation", err)
	}
}

func TestReservationDeleteFailureAfterClusterGoneRetainsSafety(t *testing.T) {
	w := newWorld(t)
	row := reservationCluster(t, w.network, "payments")
	if err := w.s.Clusters().Create(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("reservation deletion failed after CR removal")
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*corev1.ConfigMap); ok && obj.GetNamespace() == cloudstore.EKSNameReservationNamespace {
			return boom
		}
		return c.Delete(ctx, obj, opts...)
	}})
	s := cloudstore.New(k)
	if err := s.Clusters().Delete(t.Context(), "acme", row.ID); !errors.Is(err, boom) {
		t.Fatal("failed to inject final reservation-delete failure", err)
	}
	if _, err := s.Clusters().Get(t.Context(), "acme", row.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatal("Cluster removal did not precede failure", err)
	}
	// The reservation was marked releasing once the inventory was confirmed
	// empty (the cloud side is gone), before the Cluster was removed: that
	// mark, not the missing Cluster alone, lets the name be taken again.
	claims := &corev1.ConfigMapList{}
	if err := w.k.List(t.Context(), claims, client.InNamespace(cloudstore.EKSNameReservationNamespace)); err != nil || len(claims.Items) != 1 ||
		claims.Items[0].Data["releasing"] == "" {
		t.Fatalf("the reservation of a finalized cluster is not marked releasing: %v %v", claims.Items, err)
	}
	if err := clusterreconcile.New(reconcile.Deps{Store: s, Kube: k}).Reconcile(t.Context(), "acme", row.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Clusters().Create(t.Context(), reservationCluster(t, w.network, row.Name)); err != nil {
		t.Fatal("a reservation marked releasing whose Cluster is gone still blocks the name", err)
	}
}

// A Cluster object that disappears without the platform's deletion (its
// finalizer removed by hand) leaves a reservation with a cluster UID and no
// releasing mark: nothing proves the cloud side is gone, so it still fails
// closed for the verified operator procedure.
func TestReservationOfAClusterRemovedBehindThePlatformStaysClosed(t *testing.T) {
	w := newWorld(t)
	row := reservationCluster(t, w.network, "payments")
	if err := w.s.Clusters().Create(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	claims := &corev1.ConfigMapList{}
	if err := w.k.List(t.Context(), claims, client.InNamespace(cloudstore.EKSNameReservationNamespace)); err != nil || len(claims.Items) != 1 {
		t.Fatal(err)
	}
	claim := claims.Items[0]
	claim.Data["clusterUID"] = "uid-of-the-removed-object"
	claim.Data["reservedAt"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := w.k.Update(t.Context(), &claim); err != nil {
		t.Fatal(err)
	}
	obj := &cloud.Cluster{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: row.Name}, obj); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Delete(t.Context(), obj); err != nil {
		t.Fatal(err)
	}

	if err := w.s.Clusters().Create(t.Context(), reservationCluster(t, w.network, row.Name)); !errors.Is(err, repository.ErrAlreadyExists) {
		t.Fatal("a reservation without the releasing mark was reclaimed", err)
	}
}

func TestEstablishedEKSReservationUsesFreshGetWithoutClusterLists(t *testing.T) {
	w := newWorld(t)
	row := reservationCluster(t, w.network, "payments")
	if err := w.s.Clusters().Create(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	obj := &cloud.Cluster{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: row.Name}, obj); err != nil {
		t.Fatal(err)
	}
	// Fake clients do not assign UIDs as the API server does.
	obj.UID = "established-cluster-uid"
	if err := w.k.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if err := cloudstore.EnsureEKSNameReservation(t.Context(), w.s, row); err != nil {
		t.Fatal(err)
	}
	var lists atomic.Int32
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*cloud.ClusterList); ok {
			lists.Add(1)
		}
		return c.List(ctx, list, opts...)
	}})
	s := cloudstore.New(k)
	if _, err := s.Clusters().Get(t.Context(), row.TenantID, row.ID); err != nil {
		t.Fatal(err)
	}
	lists.Store(0) // row-location cache is warm, as on a settled controller pass
	for range 3 {
		if err := cloudstore.EnsureEKSNameReservation(t.Context(), s, row); err != nil {
			t.Fatal(err)
		}
	}
	if lists.Load() != 0 {
		t.Fatalf("established owner listed Cluster inventory %d times", lists.Load())
	}
	claims := &corev1.ConfigMapList{}
	if err := w.k.List(t.Context(), claims, client.InNamespace(cloudstore.EKSNameReservationNamespace)); err != nil {
		t.Fatal(err)
	}
	claimKey := client.ObjectKeyFromObject(&claims.Items[0])
	for _, field := range []string{"clusterUID", "clusterID", "key", "tenant"} {
		t.Run(field, func(t *testing.T) {
			claim := &corev1.ConfigMap{}
			if err := w.k.Get(t.Context(), claimKey, claim); err != nil {
				t.Fatal(err)
			}
			before := claim.Data[field]
			claim.Data[field] = "conflicting-value"
			if err := w.k.Update(t.Context(), claim); err != nil {
				t.Fatal(err)
			}
			if err := cloudstore.EnsureEKSNameReservation(t.Context(), s, row); !errors.Is(err, repository.ErrAlreadyExists) {
				t.Fatal("fast path ignored changed claim ownership/scope", err)
			}
			if err := w.k.Get(t.Context(), claimKey, claim); err != nil {
				t.Fatal(err)
			}
			claim.Data[field] = before
			if err := w.k.Update(t.Context(), claim); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Kubernetes removes a deleting object as soon as its last finalizer is
// removed. The explicit Delete that follows must not skip reservation release.
func TestPhysicalEKSNameReleaseAfterFinalizerRemoval(t *testing.T) {
	for _, test := range []struct {
		name              string
		deleting, foreign bool
	}{
		{name: "normal RPC deletion"},
		{name: "finalizer auto deletion", deleting: true},
		{name: "foreign finalizer retained", deleting: true, foreign: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := newWorld(t)
			row := reservationCluster(t, w.network, "finalizing")
			if err := w.s.Clusters().Create(t.Context(), row); err != nil {
				t.Fatal(err)
			}
			obj := &cloud.Cluster{}
			key := client.ObjectKey{Namespace: namespace, Name: row.Name}
			if err := w.k.Get(t.Context(), key, obj); err != nil {
				t.Fatal(err)
			}
			obj.Finalizers = []string{cloud.ClusterFinalizer}
			if test.foreign {
				obj.Finalizers = append(obj.Finalizers, "example.com/protection")
			}
			if err := w.k.Update(t.Context(), obj); err != nil {
				t.Fatal(err)
			}
			if test.deleting {
				if err := w.k.Delete(t.Context(), obj); err != nil {
					t.Fatal(err)
				}
			}
			sawDeleteNotFound := false
			k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				err := c.Delete(ctx, obj, opts...)
				if _, ok := obj.(*cloud.Cluster); ok && apierrors.IsNotFound(err) {
					sawDeleteNotFound = true
				}
				return err
			}})
			err := cloudstore.New(k).Clusters().Delete(t.Context(), row.TenantID, row.ID)
			if test.foreign {
				if !errors.Is(err, repository.ErrConflict) {
					t.Fatalf("foreign finalizer: %v", err)
				}
				if err := w.k.Get(t.Context(), key, obj); err != nil {
					t.Fatal(err)
				}
				if len(obj.Finalizers) != 1 || obj.Finalizers[0] != "example.com/protection" {
					t.Fatalf("foreign finalizer changed: %v", obj.Finalizers)
				}
			} else {
				if test.deleting && !sawDeleteNotFound {
					t.Fatal("did not reproduce automatic deletion after finalizer update")
				}
				if err != nil {
					t.Fatalf("finalized cluster deletion: %v", err)
				}
				if err := w.k.Get(t.Context(), key, obj); !apierrors.IsNotFound(err) {
					t.Fatalf("cluster still exists: %v", err)
				}
			}
			claims := &corev1.ConfigMapList{}
			if err := w.k.List(t.Context(), claims, client.InNamespace(cloudstore.EKSNameReservationNamespace)); err != nil {
				t.Fatal(err)
			}
			wantClaims := 0
			if test.foreign {
				wantClaims = 1
			}
			if len(claims.Items) != wantClaims {
				t.Fatalf("reservations = %d, want %d", len(claims.Items), wantClaims)
			}
			err = w.s.Clusters().Create(t.Context(), reservationCluster(t, w.network, row.Name))
			if test.foreign {
				if !errors.Is(err, repository.ErrAlreadyExists) {
					t.Fatalf("foreign finalizer allowed name reuse: %v", err)
				}
			} else if err != nil {
				t.Fatalf("finalized name cannot be reused: %v", err)
			}
		})
	}
}

// A real API server answers an orphan-propagation delete by setting the
// deletion timestamp and adding the garbage collector's "orphan" finalizer;
// the object goes once the collector removes it, a moment later. The fake
// client deletes at once, so the interceptor plays the API server here.
func TestPhysicalEKSNameReleasedWhenOnlyGarbageCollectorHoldsCluster(t *testing.T) {
	w := newWorld(t)
	row := reservationCluster(t, w.network, "inference")
	if err := w.s.Clusters().Create(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		cluster, ok := obj.(*cloud.Cluster)
		if !ok {
			return c.Delete(ctx, obj, opts...)
		}
		do := &client.DeleteOptions{}
		do.ApplyOptions(opts)
		if do.PropagationPolicy == nil || *do.PropagationPolicy != metav1.DeletePropagationOrphan {
			return c.Delete(ctx, obj, opts...)
		}
		live := &cloud.Cluster{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), live); err != nil {
			return err
		}
		live.Finalizers = append(live.Finalizers, metav1.FinalizerOrphanDependents)
		if err := c.Update(ctx, live); err != nil {
			return err
		}
		return c.Delete(ctx, live)
	}})
	if err := cloudstore.New(k).Clusters().Delete(t.Context(), row.TenantID, row.ID); err != nil {
		t.Fatalf("delete held only by the garbage collector: %v", err)
	}
	obj := &cloud.Cluster{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: row.Name}, obj); err != nil ||
		obj.DeletionTimestamp.IsZero() || len(obj.Finalizers) != 1 || obj.Finalizers[0] != metav1.FinalizerOrphanDependents {
		t.Fatalf("did not reproduce the API server's orphan finalizer: %v %v", err, obj.Finalizers)
	}
	claims := &corev1.ConfigMapList{}
	if err := w.k.List(t.Context(), claims, client.InNamespace(cloudstore.EKSNameReservationNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 0 {
		t.Fatalf("reservation kept after a confirmed deletion: %d claims", len(claims.Items))
	}
}

// A process that dies between reserving a name and storing its Cluster
// leaves a reservation no cluster ever got: no clusterUID, no Cluster with
// its ID. Nothing reached the cloud for it (cloud resources come only from
// a stored Cluster), so once it is clearly not an in-flight create (older
// than the abandon age) another create takes the name over. One a Cluster
// holds, or a young one, still fails closed.
func TestAbandonedEKSReservationIsReclaimed(t *testing.T) {
	for _, tt := range []struct {
		name      string
		age       time.Duration
		withUID   bool
		reclaimed bool
	}{
		{name: "abandoned", age: 11 * time.Minute, reclaimed: true},
		{name: "maybe still in flight", age: 2 * time.Minute},
		{name: "held by a cluster object", age: time.Hour, withUID: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			// The crash: the Cluster is never stored and the claim stays.
			crashed := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*cloud.Cluster); ok {
						return fmt.Errorf("process died after the reservation")
					}
					return c.Create(ctx, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*corev1.ConfigMap); ok {
						return fmt.Errorf("process died before it could release the reservation")
					}
					return c.Delete(ctx, obj, opts...)
				}})
			if err := cloudstore.New(crashed).Clusters().Create(t.Context(), reservationCluster(t, w.network, "infra")); err == nil {
				t.Fatal("simulated crash ignored")
			}
			claims := &corev1.ConfigMapList{}
			if err := w.k.List(t.Context(), claims, client.InNamespace(cloudstore.EKSNameReservationNamespace)); err != nil || len(claims.Items) != 1 {
				t.Fatalf("crash left %d reservations (%v), want 1", len(claims.Items), err)
			}
			claim := claims.Items[0]
			claim.Data["reservedAt"] = time.Now().Add(-tt.age).UTC().Format(time.RFC3339)
			if tt.withUID {
				claim.Data["clusterUID"] = "uid-of-a-cluster-object"
			}
			if err := w.k.Update(t.Context(), &claim); err != nil {
				t.Fatal(err)
			}

			err := w.s.Clusters().Create(t.Context(), reservationCluster(t, w.network, "infra"))

			if tt.reclaimed && err != nil {
				t.Fatalf("an abandoned reservation still blocks the name: %v", err)
			}
			if !tt.reclaimed && !errors.Is(err, repository.ErrAlreadyExists) {
				t.Fatalf("reservation reclaimed although it may be in use: %v", err)
			}
		})
	}
}

// strayCluster stores a Cluster named name in ns whose network cannot be
// read: its scope (account) is unknown.
func strayCluster(t *testing.T, k client.Client, ns, name, region string) {
	t.Helper()
	stray := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	stray.Spec.Region = region
	stray.Spec.Network.NetworkRef.Name = "missing"
	if err := k.Create(t.Context(), stray); err != nil {
		t.Fatal(err)
	}
}

func TestPhysicalEKSNameIgnoresClustersOutsideTenantNamespaces(t *testing.T) {
	w := newWorld(t)
	strayCluster(t, w.k, "default", "payments", w.network.Region)
	if err := w.s.Clusters().Create(t.Context(), reservationCluster(t, w.network, "payments")); err != nil {
		t.Fatalf("a Cluster in no tenant's namespace blocked the name: %v", err)
	}
}

func TestPhysicalEKSNameUnknownScopeInTenantNamespaceStaysClosed(t *testing.T) {
	w := newWorld(t)
	other := reservationPeer(t, w, "other", w.network.Region)
	strayCluster(t, w.k, "tenant-other", "payments", other.Region)
	if err := w.s.Clusters().Create(t.Context(), reservationCluster(t, w.network, "payments")); !errors.Is(err, repository.ErrAlreadyExists) {
		t.Fatalf("a tenant's Cluster of unknown scope did not fail closed: %v", err)
	}
}
