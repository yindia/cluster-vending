package kubestore

import (
	"errors"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/yindia/cluster-vending/pkg/apis/platform/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
)

const testNamespace = "cluster-vending-system"

// newFake is a store on a fake API server (it refuses a write with a
// stale resourceVersion, like the real one), and its client.
func newFake(t *testing.T) (repository.Store, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return New(c, testNamespace), c
}

// The behaviour every store has (pkg/database/storetest), on custom
// resources.
func TestStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) repository.Store {
		s, _ := newFake(t)
		return s
	})
}

func count(t *testing.T, c client.Client, list client.ObjectList) int {
	t.Helper()
	if err := c.List(t.Context(), list, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	switch l := list.(type) {
	case *platformv1alpha1.ClaimList:
		return len(l.Items)
	case *platformv1alpha1.ClusterList:
		return len(l.Items)
	case *platformv1alpha1.ManagedResourceList:
		return len(l.Items)
	}
	t.Fatalf("count of %T", list)
	return 0
}

// A row's claims go with it: nothing is left behind that would keep a
// value taken.
func TestClaimsAreReleasedWithTheirRows(t *testing.T) {
	s, c := newFake(t)
	ctx := t.Context()
	storetest.MustTenant(t, s, "acme")
	n := storetest.MustNetwork(t, s, "acme", "n")
	cluster := storetest.MustCluster(t, s, "acme", "prod", n)
	storetest.MustPool(t, s, cluster, "default")
	if got := count(t, c, &platformv1alpha1.ClaimList{}); got != 3 { // account, cluster name, node pool name
		t.Fatalf("%d claims, want 3", got)
	}
	if err := s.Clusters().Delete(ctx, "acme", cluster.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Networks().Delete(ctx, "acme", n.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tenants().Delete(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if got := count(t, c, &platformv1alpha1.ClaimList{}); got != 0 {
		t.Errorf("%d claims left after everything was deleted", got)
	}
}

// Of creates of one cluster name at the same moment exactly one is
// stored, and the others leave nothing behind.
func TestConcurrentCreatesOfOneNameStoreOne(t *testing.T) {
	s, c := newFake(t)
	storetest.MustTenant(t, s, "acme")
	n := storetest.MustNetwork(t, s, "acme", "n")
	const writers = 8
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			errs[i] = s.Clusters().Create(t.Context(), &model.Cluster{TenantID: "acme", Name: "prod", NetworkID: n.ID,
				Provider: "aws", Region: "us-east-1", Resource: model.Resource{State: "x", Spec: storetest.Spec("c")}})
		})
	}
	wg.Wait()
	stored := 0
	for _, err := range errs {
		switch {
		case err == nil:
			stored++
		case !errors.Is(err, repository.ErrAlreadyExists):
			t.Errorf("a refused create failed with %v, want ErrAlreadyExists", err)
		}
	}
	if stored != 1 || count(t, c, &platformv1alpha1.ClusterList{}) != 1 {
		t.Errorf("%d creates succeeded, %d clusters stored; want one", stored, count(t, c, &platformv1alpha1.ClusterList{}))
	}
}

// Recording the same objects at the same moment makes one record each.
func TestConcurrentPutsRecordOnce(t *testing.T) {
	s, c := newFake(t)
	objects := []model.ManagedResource{
		{TenantID: "acme", CloudAccountID: "a", OwnerKind: model.OwnerNetwork, OwnerID: "n", APIVersion: "v1", Kind: "VPC", Namespace: "tenant-acme", Name: "vpc"},
		{TenantID: "acme", CloudAccountID: "a", OwnerKind: model.OwnerNetwork, OwnerID: "n", APIVersion: "v1", Kind: "Subnet", Namespace: "tenant-acme", Name: "a"},
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if err := s.ManagedResources().Put(t.Context(), objects); err != nil {
				t.Errorf("Put: %v", err)
			}
		})
	}
	wg.Wait()
	if got := count(t, c, &platformv1alpha1.ManagedResourceList{}); got != 2 {
		t.Errorf("%d records, want 2", got)
	}
}

// A claim whose holder never got stored (a create that died half-way)
// keeps the value only for claimGrace.
func TestAbandonedClaimIsTakenAfterTheGrace(t *testing.T) {
	s, _ := newFake(t)
	ctx := t.Context()
	storetest.MustTenant(t, s, "acme")
	n := storetest.MustNetwork(t, s, "acme", "n")
	internal := s.(store)
	key := clusterKeys(&model.Cluster{CloudAccountID: n.CloudAccountID, Region: "us-east-1", Name: "prod"})[0]
	if err := internal.claim(ctx, key, kindCluster, "a-cluster-that-was-never-stored"); err != nil {
		t.Fatal(err)
	}
	create := func() error {
		return s.Clusters().Create(ctx, &model.Cluster{TenantID: "acme", Name: "prod", NetworkID: n.ID,
			Provider: "aws", Region: "us-east-1", Resource: model.Resource{State: "x", Spec: storetest.Spec("c")}})
	}
	storetest.RequireErr(t, create(), repository.ErrAlreadyExists) // its create may still be on its way

	at := time.Now().Add(2 * claimGrace)
	now = func() time.Time { return at }
	t.Cleanup(func() { now = time.Now })
	if err := create(); err != nil {
		t.Fatalf("create once the claim is abandoned: %v", err)
	}
	// The claim of a stored cluster is never taken, however old.
	at = at.Add(24 * time.Hour)
	storetest.RequireErr(t, create(), repository.ErrAlreadyExists)
}

// What a failed transaction created is taken back, with its claims; a
// transaction that succeeds keeps everything.
func TestWithinTransactionUndoesCreates(t *testing.T) {
	s, c := newFake(t)
	ctx := t.Context()
	storetest.MustTenant(t, s, "acme")
	boom := errors.New("boom")
	err := s.WithinTransaction(ctx, func(tx repository.Store) error {
		storetest.MustCluster(t, tx, "acme", "prod", storetest.MustNetwork(t, tx, "acme", "n"))
		return boom
	})
	storetest.RequireErr(t, err, boom)
	if clusters, claims := count(t, c, &platformv1alpha1.ClusterList{}), count(t, c, &platformv1alpha1.ClaimList{}); clusters != 0 || claims != 0 {
		t.Errorf("%d clusters and %d claims after a failed transaction; want none", clusters, claims)
	}
	err = s.WithinTransaction(ctx, func(tx repository.Store) error {
		storetest.MustCluster(t, tx, "acme", "prod", storetest.MustNetwork(t, tx, "acme", "n"))
		return nil
	})
	if err != nil || count(t, c, &platformv1alpha1.ClusterList{}) != 1 {
		t.Errorf("a transaction that succeeded: %v", err)
	}
}
