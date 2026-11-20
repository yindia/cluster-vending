package cloudstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

func reservations(t *testing.T, k client.Client) []corev1.ConfigMap {
	t.Helper()
	claims := &corev1.ConfigMapList{}
	if err := k.List(t.Context(), claims, client.InNamespace(cloudstore.EKSNameReservationNamespace)); err != nil {
		t.Fatal(err)
	}
	return claims.Items
}

func clusterExists(t *testing.T, k client.Client, name string) bool {
	t.Helper()
	err := k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &cloud.Cluster{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// terminate gives the (only) network of the tenant a deletionTimestamp, held
// by a finalizer, while its stored state stays what it was.
func terminate(ctx context.Context, t testing.TB, k client.Client) {
	list := &cloud.NetworkList{}
	if err := k.List(ctx, list, client.InNamespace(namespace)); err != nil || len(list.Items) != 1 {
		t.Fatal(list, err)
	}
	n := &list.Items[0]
	n.Finalizers = append(n.Finalizers, "example.com/protection")
	if err := k.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete(ctx, n); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRefusedWhileNetworkIsTerminating(t *testing.T) {
	w := newWorld(t)
	terminate(t.Context(), t, w.k)
	err := w.s.Clusters().Create(t.Context(), reservationCluster(t, w.network, "late"))
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("create in a terminating network: %v", err)
	}
	if clusterExists(t, w.k, "late") || len(reservations(t, w.k)) != 0 {
		t.Fatal("a refused create left a cluster or a reservation")
	}
}

func TestCreateRollsBackWhenNetworkStartsTerminatingMidCreate(t *testing.T) {
	w := newWorld(t)
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		if _, ok := obj.(*cloud.Cluster); ok {
			terminate(ctx, t, c)
		}
		return nil
	}})
	err := cloudstore.New(k).Clusters().Create(t.Context(), reservationCluster(t, w.network, "racing"))
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("create while the network starts terminating: %v", err)
	}
	if clusterExists(t, w.k, "racing") || len(reservations(t, w.k)) != 0 {
		t.Fatal("rolled back create left a cluster or a reservation")
	}
}

func failNetworkUpdate(k client.Client) client.Client {
	return interceptor.NewClient(k.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*cloud.Network); ok {
			return apierrors.NewInternalError(errors.New("network update failed"))
		}
		return c.Update(ctx, obj, opts...)
	}})
}

func TestFailedCreateReleasesItsReservation(t *testing.T) {
	w := newWorld(t)
	rid := uuid.NewString()
	row := reservationCluster(t, w.network, "retryable")
	row.CreateRequestID = &rid
	if err := cloudstore.New(failNetworkUpdate(w.k)).Clusters().Create(t.Context(), row); err == nil {
		t.Fatal("create succeeded although the network could not be claimed")
	}
	if clusterExists(t, w.k, "retryable") {
		t.Fatal("cluster not taken back")
	}
	if n := len(reservations(t, w.k)); n != 0 {
		t.Fatalf("reservation of a rolled back create kept: %d", n)
	}
	again := reservationCluster(t, w.network, "retryable")
	if err := w.s.Clusters().Create(t.Context(), again); err != nil {
		t.Fatalf("fresh create with the same name: %v", err)
	}
}

func TestFailedCreateReleasesReservationWhenInsertFails(t *testing.T) {
	w := newWorld(t)
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*cloud.Cluster); ok {
			return apierrors.NewInternalError(errors.New("etcd unavailable"))
		}
		return c.Create(ctx, obj, opts...)
	}})
	if err := cloudstore.New(k).Clusters().Create(t.Context(), reservationCluster(t, w.network, "noinsert")); err == nil {
		t.Fatal("create succeeded")
	}
	if n := len(reservations(t, w.k)); n != 0 {
		t.Fatalf("reservation kept after a cluster that was never stored: %d", n)
	}
	if err := w.s.Clusters().Create(t.Context(), reservationCluster(t, w.network, "noinsert")); err != nil {
		t.Fatal(err)
	}
}

// failCreateRollback fails the network claim and makes the rollback delete of
// the cluster leave it terminating, like the API server does.
func failWithHeldRollback(k client.Client, finalizer string) client.Client {
	return interceptor.NewClient(failNetworkUpdate(k).(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*cloud.Cluster); !ok {
			return c.Delete(ctx, obj, opts...)
		}
		live := &cloud.Cluster{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
			return err
		}
		live.Finalizers = append(live.Finalizers, finalizer)
		if err := c.Update(ctx, live); err != nil {
			return err
		}
		return c.Delete(ctx, live)
	}})
}

func TestFailedCreateKeepsReservationWhenRollbackIsHeldByForeignFinalizer(t *testing.T) {
	w := newWorld(t)
	err := cloudstore.New(failWithHeldRollback(w.k, "example.com/protection")).Clusters().Create(t.Context(), reservationCluster(t, w.network, "held"))
	if err == nil {
		t.Fatal("create succeeded")
	}
	if n := len(reservations(t, w.k)); n != 1 {
		t.Fatalf("reservation must stay while the rollback is unconfirmed: %d", n)
	}
}

func TestFailedCreateReleasesReservationWhenOnlyCollectorHoldsRollback(t *testing.T) {
	w := newWorld(t)
	err := cloudstore.New(failWithHeldRollback(w.k, metav1.FinalizerOrphanDependents)).Clusters().Create(t.Context(), reservationCluster(t, w.network, "orphaned"))
	if err == nil {
		t.Fatal("create succeeded")
	}
	if n := len(reservations(t, w.k)); n != 0 {
		t.Fatalf("reservation kept after a confirmed rollback: %d", n)
	}
}

func TestFailedCreateDoesNotReleaseAReservationOfAnotherAttempt(t *testing.T) {
	w := newWorld(t)
	k := interceptor.NewClient(failNetworkUpdate(w.k).(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		err := c.Create(ctx, obj, opts...)
		if _, ok := obj.(*cloud.Cluster); ok && err == nil {
			// Another actor replaces the reservation after this attempt took it.
			claims := &corev1.ConfigMapList{}
			if lerr := c.List(ctx, claims, client.InNamespace(cloudstore.EKSNameReservationNamespace)); lerr != nil || len(claims.Items) != 1 {
				t.Fatal(claims, lerr)
			}
			claim := claims.Items[0]
			if derr := c.Delete(ctx, &claim); derr != nil {
				t.Fatal(derr)
			}
			claim.ResourceVersion, claim.UID = "", ""
			claim.Data = map[string]string{"key": claim.Data["key"], "tenant": "acme", "clusterID": uuid.NewString(), "requestID": "", "requestHash": "other"}
			if cerr := c.Create(ctx, &claim); cerr != nil {
				t.Fatal(cerr)
			}
		}
		return err
	}})
	if err := cloudstore.New(k).Clusters().Create(t.Context(), reservationCluster(t, w.network, "swapped")); err == nil {
		t.Fatal("create succeeded")
	}
	if n := len(reservations(t, w.k)); n != 1 {
		t.Fatalf("a reservation that no longer belongs to the attempt was released: %d", n)
	}
}

func TestCreateCollisionOfIdenticalRetriesKeepsWinnersClaims(t *testing.T) {
	w := newWorld(t)
	rid := uuid.NewString()
	mk := func() *model.Cluster {
		c := reservationCluster(t, w.network, "twice")
		c.CreateRequestID = &rid
		return c
	}
	winner := mk()
	armed := true
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*cloud.Cluster); ok && armed {
			armed = false // the other retry stores the cluster first
			if err := w.s.Clusters().Create(ctx, winner); err != nil {
				t.Fatalf("winner: %v", err)
			}
		}
		return c.Create(ctx, obj, opts...)
	}})
	loser := mk()
	if err := cloudstore.New(k).Clusters().Create(t.Context(), loser); err != nil {
		t.Fatalf("identical retry that lost the insert: %v", err)
	}
	if loser.ID != winner.ID {
		t.Fatalf("idempotent result is %s, winner is %s", loser.ID, winner.ID)
	}
	got, err := w.s.Clusters().GetByCreateRequest(t.Context(), "acme", rid)
	if err != nil || got.ID != winner.ID {
		t.Fatalf("request ID no longer finds the result: %v %v", got, err)
	}
	if n := len(reservations(t, w.k)); n != 1 {
		t.Fatalf("winner's reservation: %d", n)
	}
}

func TestListLeavesPolicySnapshotOutAndGetFillsIt(t *testing.T) {
	w := newWorld(t)
	row := reservationCluster(t, w.network, "snap")
	if err := w.s.Clusters().Create(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	obj := &cloud.Cluster{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: "snap"}, obj); err != nil {
		t.Fatal(err)
	}
	obj.Status.PolicySnapshot = `{"violations":[]}`
	if err := w.k.Status().Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	rows, err := w.s.Clusters().List(t.Context(), "acme", repository.Page{Limit: 10})
	if err != nil || len(rows) != 1 || len(rows[0].PolicySnapshot) != 0 {
		t.Fatalf("list carries the snapshot: %v %v", rows, err)
	}
	got, err := w.s.Clusters().Get(t.Context(), "acme", row.ID)
	if err != nil || string(got.PolicySnapshot) != `{"violations":[]}` {
		t.Fatalf("get lost the snapshot: %v %v", got, err)
	}
}

// regionNetwork is a second network of the tenant, in another region of the
// same account.
func regionNetwork(t *testing.T, w world, region string) *model.Network {
	t.Helper()
	n := &model.Network{TenantID: "acme", Name: "second", Provider: model.ProviderAWS, Region: region,
		CloudAccountID: w.account, Resource: model.Resource{State: "NETWORK_STATE_PROVISIONING"}}
	n.Spec = document(t, networkSpec(n))
	if err := w.s.Networks().Create(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateCollisionWithOtherRegionReleasesOwnReservation(t *testing.T) {
	w := newWorld(t)
	other := regionNetwork(t, w, "eu-west-1")
	ridWinner, ridLoser := uuid.NewString(), uuid.NewString()
	winner := reservationCluster(t, w.network, "twice")
	winner.CreateRequestID = &ridWinner
	armed := true
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*cloud.Cluster); ok && armed {
			armed = false
			if err := w.s.Clusters().Create(ctx, winner); err != nil {
				t.Fatalf("winner: %v", err)
			}
		}
		return c.Create(ctx, obj, opts...)
	}})
	loser := reservationCluster(t, other, "twice")
	loser.CreateRequestID = &ridLoser
	err := cloudstore.New(k).Clusters().Create(t.Context(), loser)
	if !errors.Is(err, repository.ErrAlreadyExists) {
		t.Fatalf("collision: %v", err)
	}
	if n := len(reservations(t, w.k)); n != 1 {
		t.Fatalf("loser's reservation kept: %d reservations", n)
	}
	if _, err := w.s.Clusters().GetByCreateRequest(t.Context(), "acme", ridLoser); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("loser's request claim kept: %v", err)
	}
	if _, err := w.s.Clusters().GetByCreateRequest(t.Context(), "acme", ridWinner); err != nil {
		t.Fatalf("winner's request claim lost: %v", err)
	}
}

func TestCreateIdenticalRetryDoesNotSucceedForARolledBackWinner(t *testing.T) {
	w := newWorld(t)
	rid := uuid.NewString()
	mk := func() *model.Cluster {
		c := reservationCluster(t, w.network, "twice")
		c.CreateRequestID = &rid
		return c
	}
	winner := mk()
	armed, rolledBack := true, false
	k := interceptor.NewClient(w.k.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*cloud.Cluster); ok && armed {
				armed = false
				if err := w.s.Clusters().Create(ctx, winner); err != nil {
					t.Fatalf("winner: %v", err)
				}
			}
			return c.Create(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if _, ok := obj.(*cloud.Cluster); ok && err == nil && !armed && !rolledBack {
				rolledBack = true // the winner is taken back right after the loser read it
				if derr := c.Delete(ctx, obj.DeepCopyObject().(client.Object)); derr != nil {
					t.Fatal(derr)
				}
			}
			return err
		}})
	err := cloudstore.New(k).Clusters().Create(t.Context(), mk())
	if !rolledBack {
		t.Fatal("the winner was never taken back by the test")
	}
	if err == nil || !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("retry reported %v for a cluster that is gone", err)
	}
}
