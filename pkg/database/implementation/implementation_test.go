package implementation_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yindia/cluster-vending/pkg/database"
	"github.com/yindia/cluster-vending/pkg/database/implementation"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// newStore opens a fresh, migrated database for one test: SQLite by
// default, PostgreSQL when CV_TEST_POSTGRES_DSN is set (each test then gets
// its own schema, dropped afterwards), e.g.
//
//	CV_TEST_POSTGRES_DSN='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
func newStore(t *testing.T) repository.Store {
	t.Helper()
	s, _ := newStoreDB(t)
	return s
}

// newStoreDB also returns the database, for checks that look past soft
// deletes (Unscoped).
func newStoreDB(t *testing.T) (repository.Store, *gorm.DB) {
	t.Helper()
	cfg := database.Config{Driver: database.DriverSQLite, DSN: filepath.Join(t.TempDir(), "cv.db")}
	if dsn := os.Getenv("CV_TEST_POSTGRES_DSN"); dsn != "" {
		cfg = database.Config{Driver: database.DriverPostgres, DSN: postgresSchema(t, dsn)}
	}
	db, err := database.Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return implementation.New(db), db
}

// postgresSchema creates a schema for one test and returns a DSN using it.
func postgresSchema(t *testing.T, dsn string) string {
	t.Helper()
	admin, err := database.Open(t.Context(), database.Config{Driver: database.DriverPostgres, DSN: dsn})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = database.Close(admin)
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}

func spec(kind string) model.JSON { return model.JSON(fmt.Sprintf(`{"kind":%q}`, kind)) }

func mustTenant(t *testing.T, s repository.Store, id string) {
	t.Helper()
	if err := s.Tenants().Create(t.Context(), &model.Tenant{ID: id}); err != nil {
		t.Fatalf("create tenant %s: %v", id, err)
	}
}

// mustAccount bootstraps the tenant's AWS account 444455556666 (again) and
// returns its row ID.
func mustAccount(t *testing.T, s repository.Store, tenant string) string {
	t.Helper()
	a := &model.CloudAccount{TenantID: tenant, Provider: model.ProviderAWS, AccountID: "444455556666"}
	if err := s.Tenants().PutCloudAccount(t.Context(), a); err != nil {
		t.Fatalf("put account: %v", err)
	}
	got, err := s.Tenants().GetCloudAccount(t.Context(), tenant, model.ProviderAWS, "444455556666")
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return got.ID
}

func mustNetwork(t *testing.T, s repository.Store, tenant, name string) *model.Network {
	t.Helper()
	n := &model.Network{
		TenantID: tenant, Name: name, Provider: "aws", Region: "us-east-1", CloudAccountID: mustAccount(t, s, tenant),
		Resource: model.Resource{State: "NETWORK_STATE_READY", Spec: spec("network")},
	}
	if err := s.Networks().Create(t.Context(), n); err != nil {
		t.Fatalf("create network %s: %v", name, err)
	}
	return n
}

func mustCluster(t *testing.T, s repository.Store, tenant, name string, network *model.Network) *model.Cluster {
	t.Helper()
	c := &model.Cluster{
		TenantID: tenant, Name: name, NetworkID: network.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "CLUSTER_STATE_RUNNING", Spec: spec("cluster")},
	}
	if err := s.Clusters().Create(t.Context(), c); err != nil {
		t.Fatalf("create cluster %s: %v", name, err)
	}
	return c
}

func mustPool(t *testing.T, s repository.Store, cluster *model.Cluster, name string) *model.NodePool {
	t.Helper()
	p := &model.NodePool{
		ClusterID: cluster.ID, Name: name,
		Resource: model.Resource{State: "NODE_POOL_STATE_RUNNING", Spec: spec("pool")},
	}
	if err := s.NodePools().Create(t.Context(), p); err != nil {
		t.Fatalf("create node pool %s: %v", name, err)
	}
	return p
}

func requireErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestCreateAssignsIDAndETagAndRoundTrips(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	n := mustNetwork(t, s, "acme", "shared-use1")
	if n.ID == "" || n.ETag == "" || n.Generation != 1 {
		t.Fatalf("create did not assign ID / ETag / generation: %+v", n.Resource)
	}

	got, err := s.Networks().Get(t.Context(), "acme", n.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var stored map[string]string
	if err := json.Unmarshal(got.Spec, &stored); err != nil {
		t.Fatalf("stored spec: %v", err)
	}
	// Compare meaning, not bytes: Postgres jsonb normalizes whitespace.
	if got.ID != n.ID || stored["kind"] != "network" || got.Region != "us-east-1" {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestNetworkNamesAreNotUnique(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	a, b := mustNetwork(t, s, "acme", "prod"), mustNetwork(t, s, "acme", "prod")
	if a.ID == b.ID {
		t.Fatal("two networks got one ID")
	}
	if a.ID >= b.ID {
		t.Errorf("IDs %s, %s are not time-ordered", a.ID, b.ID)
	}
}

func TestClusterNamesAreNotUnique(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	n := mustNetwork(t, s, "acme", "n")
	a, b := mustCluster(t, s, "acme", "prod", n), mustCluster(t, s, "acme", "prod", n)
	if a.ID == b.ID {
		t.Fatal("two clusters got one ID")
	}
	got, err := s.Clusters().Get(t.Context(), "acme", b.ID)
	if err != nil || got.Name != "prod" {
		t.Fatalf("Get by ID = %+v, %v", got, err)
	}
}

func TestCreateRequestIDIsIdempotencyKey(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	key := "3f2b8c1e-4a5d-4e6f-9a7b-1c2d3e4f5a6b"
	n := &model.Network{TenantID: "acme", Name: "a", Provider: "aws", Region: "us-east-1", CreateRequestID: &key, CloudAccountID: mustAccount(t, s, "acme"),
		Resource: model.Resource{State: "x", Spec: spec("n")}}
	if err := s.Networks().Create(t.Context(), n); err != nil {
		t.Fatalf("Create: %v", err)
	}

	again := &model.Network{TenantID: "acme", Name: "b", Provider: "aws", Region: "us-east-1", CreateRequestID: &key, CloudAccountID: mustAccount(t, s, "acme"),
		Resource: model.Resource{State: "x", Spec: spec("n")}}
	requireErr(t, s.Networks().Create(t.Context(), again), repository.ErrAlreadyExists)

	got, err := s.Networks().GetByCreateRequest(t.Context(), "acme", key)
	if err != nil || got.Name != "a" {
		t.Fatalf("GetByCreateRequest = %v, %v; want network a", got, err)
	}
	mustNetwork(t, s, "acme", "c") // no request ID: NULLs never collide
	mustNetwork(t, s, "acme", "d")
}

func TestParentsMustExist(t *testing.T) {
	s := newStore(t)
	n := &model.Network{TenantID: "nobody", Name: "x", Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "x", Spec: spec("n")}}
	requireErr(t, s.Networks().Create(t.Context(), n), repository.ErrNotFound)

	mustTenant(t, s, "acme")
	c := &model.Cluster{TenantID: "acme", Name: "x", NetworkID: "no-such-network", Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "x", Spec: spec("c")}}
	requireErr(t, s.Clusters().Create(t.Context(), c), repository.ErrNotFound)
}

func TestNetworkInUseCannotBeDeleted(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	n := mustNetwork(t, s, "acme", "shared")
	mustCluster(t, s, "acme", "a", n)
	mustCluster(t, s, "acme", "b", n)

	requireErr(t, s.Networks().Delete(t.Context(), "acme", n.ID), repository.ErrInUse)

	users, err := s.Clusters().ListByNetwork(t.Context(), n.ID)
	if err != nil || len(users) != 2 {
		t.Fatalf("ListByNetwork = %d clusters, %v; want 2", len(users), err)
	}
	for _, c := range users {
		if err := s.Clusters().Delete(t.Context(), "acme", c.ID); err != nil {
			t.Fatalf("delete cluster %s: %v", c.Name, err)
		}
	}
	if err := s.Networks().Delete(t.Context(), "acme", n.ID); err != nil {
		t.Fatalf("delete unused network: %v", err)
	}
}

func TestDeletingClusterDeletesItsNodePools(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	c := mustCluster(t, s, "acme", "prod", mustNetwork(t, s, "acme", "n"))
	mustPool(t, s, c, "default")
	mustPool(t, s, c, "gpu")

	if err := s.Clusters().Delete(t.Context(), "acme", c.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	pools, err := s.NodePools().List(t.Context(), c.ID, repository.Page{Limit: 10})
	if err != nil || len(pools) != 0 {
		t.Fatalf("node pools after cluster delete = %d, %v; want 0", len(pools), err)
	}
}

func TestNodePoolNamesAreUniquePerCluster(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	n := mustNetwork(t, s, "acme", "n")
	a, b := mustCluster(t, s, "acme", "a", n), mustCluster(t, s, "acme", "b", n)
	mustPool(t, s, a, "default")
	mustPool(t, s, b, "default")

	dup := &model.NodePool{ClusterID: a.ID, Name: "default", Resource: model.Resource{State: "x", Spec: spec("p")}}
	requireErr(t, s.NodePools().Create(t.Context(), dup), repository.ErrAlreadyExists)
}

func TestTenantWithResourcesCannotBeDeleted(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	mustNetwork(t, s, "acme", "n")
	requireErr(t, s.Tenants().Delete(t.Context(), "acme"), repository.ErrInUse)

	mustTenant(t, s, "empty")
	if err := s.Tenants().PutCloudAccount(t.Context(), &model.CloudAccount{TenantID: "empty", Provider: "aws", AccountID: "444455556666"}); err != nil {
		t.Fatalf("PutCloudAccount: %v", err)
	}
	if err := s.Tenants().Delete(t.Context(), "empty"); err != nil {
		t.Fatalf("delete tenant with only cloud accounts: %v", err)
	}
	_, err := s.Tenants().GetCloudAccount(t.Context(), "empty", model.ProviderAWS, "444455556666")
	requireErr(t, err, repository.ErrNotFound) // cascaded
}

func TestTenantHasManyCloudAccounts(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	put := func(account, name string) *model.CloudAccount {
		t.Helper()
		a := &model.CloudAccount{TenantID: "acme", Provider: "aws", AccountID: account, AccountName: name,
			Namespace: "tenant-acme", Identity: "role-" + name}
		if err := s.Tenants().PutCloudAccount(t.Context(), a); err != nil {
			t.Fatalf("PutCloudAccount: %v", err)
		}
		got, err := s.Tenants().GetCloudAccount(t.Context(), "acme", model.ProviderAWS, account)
		if err != nil {
			t.Fatalf("GetCloudAccount: %v", err)
		}
		return got
	}
	prod := put("444455556666", "acme-prod")
	staging := put("777788889999", "acme-staging") // adds, does not replace
	if prod.ProviderConfigName == "" || prod.ProviderConfigName == staging.ProviderConfigName {
		t.Errorf("provider config names = %q, %q: want generated and distinct", prod.ProviderConfigName, staging.ProviderConfigName)
	}

	again := put("444455556666", "acme production") // re-bootstrap
	if again.ID != prod.ID || again.AccountName != "acme production" || again.ProviderConfigName != prod.ProviderConfigName ||
		again.Identity != "role-acme production" {
		t.Errorf("re-put = %+v, want the same row with the new name and the same provider config", again)
	}
	all, err := s.Tenants().ListCloudAccounts(t.Context(), "acme")
	if err != nil || len(all) != 2 {
		t.Fatalf("ListCloudAccounts = %d, %v", len(all), err)
	}
	byID, err := s.Tenants().GetCloudAccountByID(t.Context(), "acme", staging.ID)
	if err != nil || byID.AccountID != "777788889999" {
		t.Fatalf("GetCloudAccountByID = %+v, %v", byID, err)
	}
	_, err = s.Tenants().GetCloudAccountByID(t.Context(), "globex", staging.ID) // another tenant
	requireErr(t, err, repository.ErrNotFound)
	requireErr(t, s.Tenants().PutCloudAccount(t.Context(), &model.CloudAccount{TenantID: "nobody", Provider: "aws", AccountID: "444455556666"}), repository.ErrNotFound)
}

func TestNetworkMustBeInItsTenantsAccount(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	mustTenant(t, s, "globex")
	globexAccount := mustAccount(t, s, "globex")
	n := &model.Network{TenantID: "acme", Name: "n", Provider: "aws", Region: "us-east-1", CloudAccountID: globexAccount,
		Resource: model.Resource{State: "x", Spec: spec("n")}}
	requireErr(t, s.Networks().Create(t.Context(), n), repository.ErrInvalid)

	n.CloudAccountID = "00000000-0000-0000-0000-000000000000"
	requireErr(t, s.Networks().Create(t.Context(), n), repository.ErrNotFound)
}

func TestUpdateChecksETag(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	n := mustNetwork(t, s, "acme", "n")
	stale := *n

	n.DisplayName = "Shared"
	n.Generation = 2
	if err := s.Networks().Update(t.Context(), n); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if n.ETag == stale.ETag {
		t.Fatal("Update did not assign a new etag")
	}

	stale.DisplayName = "lost update"
	requireErr(t, s.Networks().Update(t.Context(), &stale), repository.ErrConflict)

	got, _ := s.Networks().Get(t.Context(), "acme", n.ID)
	if got.DisplayName != "Shared" || got.Generation != 2 {
		t.Fatalf("stored = %+v", got.Resource)
	}

	gone := *n
	gone.ID = "no-such-row"
	requireErr(t, s.Networks().Update(t.Context(), &gone), repository.ErrNotFound)
}

func TestListPagesNetworksOldestFirst(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	mustTenant(t, s, "globex")
	for _, name := range []string{"c", "a", "e", "b", "d"} {
		mustNetwork(t, s, "acme", name)
	}
	mustNetwork(t, s, "globex", "zzz")

	var names []string
	page := repository.Page{Limit: 2}
	for {
		got, err := s.Networks().List(t.Context(), "acme", page)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, n := range got {
			names = append(names, n.Name)
		}
		if len(got) < page.Limit {
			break
		}
		page.After = got[len(got)-1].ID
	}
	if fmt.Sprint(names) != "[c a e b d]" {
		t.Fatalf("names = %v, want creation order", names)
	}

	_, err := s.Networks().List(t.Context(), "acme", repository.Page{})
	if err == nil {
		t.Fatal("List accepted a zero page size")
	}
}

func TestNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.Clusters().Get(t.Context(), "acme", "missing")
	requireErr(t, err, repository.ErrNotFound)
	requireErr(t, s.NodePools().Delete(t.Context(), "no-cluster", "x"), repository.ErrNotFound)
	_, err = s.Tenants().Get(t.Context(), "missing")
	requireErr(t, err, repository.ErrNotFound)
}

func TestWithinTransactionRollsBack(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	boom := errors.New("boom")

	var id string
	err := s.WithinTransaction(t.Context(), func(tx repository.Store) error {
		n := mustNetwork(t, tx, "acme", "n")
		id = n.ID
		mustCluster(t, tx, "acme", "c", n)
		return boom
	})

	requireErr(t, err, boom)
	_, err = s.Networks().Get(t.Context(), "acme", id)
	requireErr(t, err, repository.ErrNotFound)
}

func TestDeleteIsSoftAndNamesCanBeReused(t *testing.T) {
	s, db := newStoreDB(t)
	mustTenant(t, s, "acme")
	first := mustNetwork(t, s, "acme", "prod")

	if err := s.Networks().Delete(t.Context(), "acme", first.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := s.Networks().Get(t.Context(), "acme", first.ID)
	requireErr(t, err, repository.ErrNotFound)
	requireErr(t, s.Networks().Delete(t.Context(), "acme", first.ID), repository.ErrNotFound)

	var kept model.Network
	if err := db.Unscoped().Where("id = ?", first.ID).Take(&kept).Error; err != nil {
		t.Fatalf("deleted row is gone: %v", err)
	}
	if !kept.DeletedAt.Valid {
		t.Fatal("deleted row has no deleted_at")
	}

	second := mustNetwork(t, s, "acme", "prod") // the name is free again
	if second.ID == first.ID {
		t.Fatal("recreate reused the deleted row")
	}
}

func TestDeletedParentsTakeNoChildren(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	n := mustNetwork(t, s, "acme", "n")
	c := mustCluster(t, s, "acme", "c", n)
	if err := s.Clusters().Delete(t.Context(), "acme", c.ID); err != nil {
		t.Fatalf("delete cluster: %v", err)
	}

	pool := &model.NodePool{ClusterID: c.ID, Name: "late", Resource: model.Resource{State: "x", Spec: spec("p")}}
	requireErr(t, s.NodePools().Create(t.Context(), pool), repository.ErrNotFound)

	if err := s.Networks().Delete(t.Context(), "acme", n.ID); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	late := &model.Cluster{TenantID: "acme", Name: "late", NetworkID: n.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "x", Spec: spec("c")}}
	requireErr(t, s.Clusters().Create(t.Context(), late), repository.ErrNotFound)
}

func TestClusterMustMatchItsNetwork(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	mustTenant(t, s, "globex")
	n := mustNetwork(t, s, "acme", "n") // aws

	otherTenant := &model.Cluster{TenantID: "globex", Name: "c", NetworkID: n.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "x", Spec: spec("c")}}
	requireErr(t, s.Clusters().Create(t.Context(), otherTenant), repository.ErrInvalid)

	otherCloud := &model.Cluster{TenantID: "acme", Name: "c", NetworkID: n.ID, Provider: "gcp", Region: "us-central1",
		Resource: model.Resource{State: "x", Spec: spec("c")}}
	requireErr(t, s.Clusters().Create(t.Context(), otherCloud), repository.ErrInvalid)
}

func TestEveryProviderIsSupported(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	accounts := []model.CloudAccount{
		{Provider: model.ProviderAWS, AccountID: "444455556666"},
		{Provider: model.ProviderGCP, AccountID: "acme-prod-1234"},
		{Provider: model.ProviderAzure, AccountID: "0b1f6b2c-3d4e-4f5a-8b6c-7d8e9f0a1b2c", DirectoryID: "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"},
	}
	for _, a := range accounts {
		a.TenantID = "acme"
		if err := s.Tenants().PutCloudAccount(t.Context(), &a); err != nil {
			t.Fatalf("PutCloudAccount(%s): %v", a.Provider, err)
		}
		stored, err := s.Tenants().GetCloudAccount(t.Context(), "acme", a.Provider, a.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		n := &model.Network{TenantID: "acme", Name: string(a.Provider), Provider: a.Provider, Region: "r1", CloudAccountID: stored.ID,
			Resource: model.Resource{State: "x", Spec: spec("n")}}
		if err := s.Networks().Create(t.Context(), n); err != nil {
			t.Fatalf("network on %s: %v", a.Provider, err)
		}
	}
	got, err := s.Tenants().GetCloudAccount(t.Context(), "acme", model.ProviderAzure, "0b1f6b2c-3d4e-4f5a-8b6c-7d8e9f0a1b2c")
	if err != nil || got.DirectoryID == "" {
		t.Fatalf("azure account = %+v, %v", got, err)
	}
}

func TestProviderRulesAreEnforced(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	bad := []model.CloudAccount{
		{Provider: "openstack", AccountID: "x"},
		{Provider: model.ProviderAWS, AccountID: "1234"},
		{Provider: model.ProviderAWS, AccountID: "444455556666", DirectoryID: "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"},
		{Provider: model.ProviderGCP, AccountID: "Acme_Prod"},
		{Provider: model.ProviderAzure, AccountID: "0b1f6b2c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"}, // no directory
		{Provider: model.ProviderAzure, AccountID: "not-a-uuid", DirectoryID: "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"},
	}
	for _, a := range bad {
		a.TenantID = "acme"
		if err := s.Tenants().PutCloudAccount(t.Context(), &a); !errors.Is(err, repository.ErrInvalid) {
			t.Errorf("PutCloudAccount(%+v) = %v, want ErrInvalid", a, err)
		}
	}
	n := &model.Network{TenantID: "acme", Name: "n", Provider: "openstack", Region: "r1", CloudAccountID: mustAccount(t, s, "acme"),
		Resource: model.Resource{State: "x", Spec: spec("n")}}
	requireErr(t, s.Networks().Create(t.Context(), n), repository.ErrInvalid)
}

func TestListCloudAccounts(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	for _, a := range []model.CloudAccount{
		{Provider: model.ProviderGCP, AccountID: "acme-prod-1234"},
		{Provider: model.ProviderAWS, AccountID: "444455556666"},
	} {
		a.TenantID = "acme"
		if err := s.Tenants().PutCloudAccount(t.Context(), &a); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Tenants().ListCloudAccounts(t.Context(), "acme")
	if err != nil || len(got) != 2 || got[0].Provider != model.ProviderAWS {
		t.Fatalf("ListCloudAccounts = %+v, %v; want aws then gcp", got, err)
	}
	if none, _ := s.Tenants().ListCloudAccounts(t.Context(), "nobody"); len(none) != 0 {
		t.Errorf("unknown tenant has accounts: %+v", none)
	}
}

func TestTenantDisplayNameDefaultsToID(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	got, err := s.Tenants().Get(t.Context(), "acme")
	if err != nil || got.DisplayName != "acme" {
		t.Fatalf("tenant = %+v, %v; want display name acme", got, err)
	}
	if err := s.Tenants().Create(t.Context(), &model.Tenant{ID: "globex", DisplayName: "Globex Corp"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Tenants().Get(t.Context(), "globex"); got.DisplayName != "Globex Corp" {
		t.Errorf("given display name replaced: %q", got.DisplayName)
	}
}

func TestManagedResourcesByOwnerAndAccount(t *testing.T) {
	s := newStore(t)
	mustTenant(t, s, "acme")
	account := mustAccount(t, s, "acme")
	n := mustNetwork(t, s, "acme", "prod")
	obj := func(owner model.OwnerKind, ownerID, kind, name string) model.ManagedResource {
		return model.ManagedResource{TenantID: "acme", CloudAccountID: account, OwnerKind: owner, OwnerID: ownerID,
			APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: kind, Namespace: "tenant-acme", Name: name}
	}
	ctx := t.Context()
	if err := s.ManagedResources().Put(ctx, []model.ManagedResource{
		obj(model.OwnerCloudAccount, account, "ProviderConfig", "aws-1"),
	}); err != nil {
		t.Fatal(err)
	}
	network := []model.ManagedResource{obj(model.OwnerNetwork, n.ID, "VPC", "prod-vpc"), obj(model.OwnerNetwork, n.ID, "Subnet", "prod-a")}
	for range 2 { // recording again is a no-op
		if err := s.ManagedResources().Put(ctx, network); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, n.ID)
	if err != nil || len(got) != 2 || got[0].Name != "prod-vpc" {
		t.Fatalf("ListByOwner = %+v, %v; want VPC then subnet", got, err)
	}
	all, err := s.ManagedResources().ListByAccount(ctx, "acme", account)
	if err != nil || len(all) != 3 {
		t.Fatalf("ListByAccount = %d, %v; want 3", len(all), err)
	}
	if err := s.ManagedResources().DeleteByOwner(ctx, model.OwnerNetwork, n.ID); err != nil {
		t.Fatal(err)
	}
	if all, _ = s.ManagedResources().ListByAccount(ctx, "acme", account); len(all) != 1 || all[0].Kind != "ProviderConfig" {
		t.Errorf("after DeleteByOwner = %+v; want only the account's own", all)
	}
	if err := s.ManagedResources().Put(ctx, network); err != nil { // recreated after a delete
		t.Fatal(err)
	}
	bad := obj("bucket", n.ID, "VPC", "x")
	if err := s.ManagedResources().Put(ctx, []model.ManagedResource{bad}); !errors.Is(err, repository.ErrInvalid) {
		t.Errorf("unknown owner kind = %v, want ErrInvalid", err)
	}
}
