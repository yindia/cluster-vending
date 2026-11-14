// Package storetest is the behaviour a repository.Store must have, as
// tests. The store on the platform's own kinds (pkg/database/cloudstore)
// runs it.
package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// Factory returns a new, empty store for one test.
type Factory func(t *testing.T) repository.Store

// Backend is a store implementation under test.
type Backend struct {
	// New returns a new, empty store for one test.
	New Factory
	// NetworkSpec and ClusterSpec are the spec documents a row is created
	// with where the test reads the spec back. Nil: a stand-in document (a
	// store that keeps the spec as a document keeps any). A store that
	// keeps the spec typed gives a real contract spec for the row.
	NetworkSpec func(*model.Network) model.JSON
	ClusterSpec func(*model.Cluster) model.JSON
}

// Run runs every behaviour test against the backend's stores.
func Run(t *testing.T, b Backend) {
	t.Helper()
	for _, tc := range []struct {
		name string
		test func(*testing.T, Backend)
	}{
		{"CreateAssignsIDAndETagAndRoundTrips", testCreateAssignsIDAndETagAndRoundTrips},
		{"NetworkNamesAreUniquePerTenant", testNetworkNamesAreUniquePerTenant},
		{"ClusterNamesAreUniquePerTenant", testClusterNamesAreUniquePerTenant},
		{"ClusterCreateAndNetworkDeleteCannotBothWin", testClusterCreateAndNetworkDeleteCannotBothWin},
		{"CreateRequestIDIsIdempotencyKey", testCreateRequestIDIsIdempotencyKey},
		{"ParentsMustExist", testParentsMustExist},
		{"NetworkInUseCannotBeDeleted", testNetworkInUseCannotBeDeleted},
		{"TenantWithResourcesCannotBeDeleted", testTenantWithResourcesCannotBeDeleted},
		{"TenantHasManyCloudAccounts", testTenantHasManyCloudAccounts},
		{"NetworkMustBeInItsTenantsAccount", testNetworkMustBeInItsTenantsAccount},
		{"UpdateChecksETag", testUpdateChecksETag},
		{"ListPagesNetworksOldestFirst", testListPagesNetworksOldestFirst},
		{"NotFound", testNotFound},
		{"DeletedParentsTakeNoChildren", testDeletedParentsTakeNoChildren},
		{"ClusterMustMatchItsNetwork", testClusterMustMatchItsNetwork},
		{"OnlyAWSAccounts", testOnlyAWSAccounts},
		{"ProviderRulesAreEnforced", testProviderRulesAreEnforced},
		{"ListCloudAccounts", testListCloudAccounts},
		{"TenantDisplayNameDefaultsToID", testTenantDisplayNameDefaultsToID},
		{"ManagedResourcesByOwnerAndAccount", testManagedResourcesByOwnerAndAccount},
		{"SetStatusOnlyForTheRowItWasObservedFor", testSetStatusOnlyForTheRowItWasObservedFor},
		{"ImportedVPCIsUniquePerAccountAndRegion", testImportedVPCIsUniquePerAccountAndRegion},
		{"DeletedRowsAreGoneAndTheirNamesFree", testDeletedRowsAreGoneAndTheirNamesFree},
		{"ManagedResourceRecords", testManagedResourceRecords},
		{"ClusterLookups", testClusterLookups},
		{"PauseIsRecordedInheritedAndRemoved", testPauseIsRecordedInheritedAndRemoved},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.test(t, b) })
	}
	for _, tc := range componentTests {
		t.Run(tc.name, func(t *testing.T) { tc.test(t, b) })
	}
}

// Spec is a stand-in for a protojson spec document.
func Spec(kind string) model.JSON { return model.JSON(fmt.Sprintf(`{"kind":%q}`, kind)) }

// MustTenant creates the tenant id.
func MustTenant(t *testing.T, s repository.Store, id string) {
	t.Helper()
	if err := s.Tenants().Create(t.Context(), &model.Tenant{ID: id}); err != nil {
		t.Fatalf("create tenant %s: %v", id, err)
	}
}

// MustAccount bootstraps the tenant's AWS account 444455556666 (again) and
// returns its row ID.
func MustAccount(t *testing.T, s repository.Store, tenant string) string {
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

// MustNetwork creates a READY aws network of the name in the tenant's
// account, in us-east-1.
func MustNetwork(t *testing.T, s repository.Store, tenant, name string) *model.Network {
	t.Helper()
	n := &model.Network{
		TenantID: tenant, Name: name, Provider: "aws", Region: "us-east-1", CloudAccountID: MustAccount(t, s, tenant),
		Resource: model.Resource{State: "NETWORK_STATE_READY", Spec: Spec("network")},
	}
	if err := s.Networks().Create(t.Context(), n); err != nil {
		t.Fatalf("create network %s: %v", name, err)
	}
	return n
}

// MustCluster creates a RUNNING aws cluster of the name in the network,
// in us-east-1.
func MustCluster(t *testing.T, s repository.Store, tenant, name string, network *model.Network) *model.Cluster {
	t.Helper()
	c := &model.Cluster{
		TenantID: tenant, Name: name, NetworkID: network.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "CLUSTER_STATE_RUNNING", Spec: Spec("cluster")},
	}
	if err := s.Clusters().Create(t.Context(), c); err != nil {
		t.Fatalf("create cluster %s: %v", name, err)
	}
	return c
}

// RequireErr fails the test unless err is want (errors.Is).
func RequireErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func testCreateAssignsIDAndETagAndRoundTrips(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	n := &model.Network{TenantID: "acme", Name: "shared-use1", Provider: "aws", Region: "us-east-1", CloudAccountID: MustAccount(t, s, "acme"),
		Resource: model.Resource{State: "NETWORK_STATE_READY", Spec: Spec("network")}}
	if b.NetworkSpec != nil {
		n.Spec = b.NetworkSpec(n)
	}
	if err := s.Networks().Create(ctx, n); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n.ID == "" || n.ETag == "" || n.Generation != 1 {
		t.Fatalf("create did not assign ID / ETag / generation: %+v", n.Resource)
	}
	got, err := s.Networks().Get(ctx, "acme", n.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Compare meaning, not bytes: the store normalizes documents.
	if got.ID != n.ID || !sameDocument(got.Spec, n.Spec) || got.Region != "us-east-1" || got.Name != "shared-use1" {
		t.Fatalf("round trip = %+v\nspec %s\nwant %s", got, got.Spec, n.Spec)
	}

	c := &model.Cluster{TenantID: "acme", Name: "prod", NetworkID: n.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "CLUSTER_STATE_RUNNING", Spec: Spec("cluster")}}
	if b.ClusterSpec != nil {
		c.Spec = b.ClusterSpec(c)
	}
	if err := s.Clusters().Create(ctx, c); err != nil {
		t.Fatalf("Create cluster: %v", err)
	}
	stored, err := s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil || !sameDocument(stored.Spec, c.Spec) || stored.NetworkID != n.ID || stored.Name != "prod" {
		t.Fatalf("cluster round trip = %+v (%v)\nspec %s\nwant %s", stored, err, stored.Spec, c.Spec)
	}
}

// sameDocument reports whether two JSON documents say the same.
func sameDocument(a, b model.JSON) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// A network's name is taken once per tenant, in any region; another
// tenant has its own names.
func testNetworkNamesAreUniquePerTenant(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	first := MustNetwork(t, s, "acme", "prod")
	again := &model.Network{TenantID: "acme", Name: "prod", Provider: "aws", Region: "eu-west-1", CloudAccountID: first.CloudAccountID,
		Resource: model.Resource{State: "NETWORK_STATE_READY", Spec: Spec("network")}}
	RequireErr(t, s.Networks().Create(t.Context(), again), repository.ErrAlreadyExists)
	second := MustNetwork(t, s, "acme", "staging")
	if first.ID == second.ID {
		t.Fatal("two networks got one ID")
	}
	if first.ID >= second.ID {
		t.Errorf("IDs %s, %s are not time-ordered", first.ID, second.ID)
	}
	MustTenant(t, s, "globex")
	MustNetwork(t, s, "globex", "prod")
}

// A cluster's name is taken once per tenant, in any region (it is also
// the EKS cluster's name), and free again once that cluster is deleted.
// A cluster's name is the cloud cluster's name: one per account and
// region, also when two creates race (the index decides, not a check).
func testClusterNamesAreUniquePerTenant(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	n := MustNetwork(t, s, "acme", "n")
	first := MustCluster(t, s, "acme", "prod", n)
	if first.CloudAccountID != n.CloudAccountID || first.CloudAccountID == "" {
		t.Fatalf("cluster account = %q, want its network's %q", first.CloudAccountID, n.CloudAccountID)
	}
	again := &model.Cluster{TenantID: "acme", Name: "prod", NetworkID: n.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "CLUSTER_STATE_PROVISIONING", Spec: Spec("cluster")}}
	RequireErr(t, s.Clusters().Create(t.Context(), again), repository.ErrAlreadyExists)

	// Another name is free; the same name in another region is not.
	MustCluster(t, s, "acme", "staging", n)
	elsewhere := &model.Cluster{TenantID: "acme", Name: "prod", NetworkID: n.ID, Provider: "aws", Region: "eu-west-1",
		Resource: model.Resource{State: "CLUSTER_STATE_PROVISIONING", Spec: Spec("cluster")}}
	RequireErr(t, s.Clusters().Create(t.Context(), elsewhere), repository.ErrAlreadyExists)
	// Once the cluster is deleted its name can be used again.
	if err := s.Clusters().Delete(t.Context(), "acme", first.ID); err != nil {
		t.Fatal(err)
	}
	again.ID = ""
	if err := s.Clusters().Create(t.Context(), again); err != nil {
		t.Fatalf("name of a deleted cluster: %v", err)
	}
}

// A cluster create and its network's delete that race: exactly one wins.
func testClusterCreateAndNetworkDeleteCannotBothWin(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	n := MustNetwork(t, s, "acme", "n")
	cluster := func(name string) *model.Cluster {
		return &model.Cluster{TenantID: "acme", Name: name, NetworkID: n.ID, Provider: "aws", Region: "us-east-1",
			Resource: model.Resource{State: "CLUSTER_STATE_PROVISIONING", Spec: Spec("cluster")}}
	}

	// The delete read the network (and found no cluster), then a create
	// commits: the delete's write is refused.
	deleting, err := s.Networks().Get(ctx, "acme", n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Clusters().Create(ctx, cluster("first")); err != nil {
		t.Fatalf("create: %v", err)
	}
	deleting.State = "NETWORK_STATE_DELETING"
	RequireErr(t, s.Networks().Update(ctx, deleting), repository.ErrConflict)

	// The delete commits first: a create is refused, and stores nothing.
	current, err := s.Networks().Get(ctx, "acme", n.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.State = "NETWORK_STATE_DELETING"
	if err := s.Networks().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	RequireErr(t, s.Clusters().Create(ctx, cluster("second")), repository.ErrConflict)
	rows, err := s.Clusters().ListByNetwork(ctx, n.ID)
	if err != nil || len(rows) != 1 {
		t.Errorf("%d clusters in the network (%v), want only the first", len(rows), err)
	}
}

func testCreateRequestIDIsIdempotencyKey(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	key := "3f2b8c1e-4a5d-4e6f-9a7b-1c2d3e4f5a6b"
	n := &model.Network{TenantID: "acme", Name: "a", Provider: "aws", Region: "us-east-1", CreateRequestID: &key, CloudAccountID: MustAccount(t, s, "acme"),
		Resource: model.Resource{State: "x", Spec: Spec("n")}}
	if err := s.Networks().Create(t.Context(), n); err != nil {
		t.Fatalf("Create: %v", err)
	}

	again := &model.Network{TenantID: "acme", Name: "b", Provider: "aws", Region: "us-east-1", CreateRequestID: &key, CloudAccountID: MustAccount(t, s, "acme"),
		Resource: model.Resource{State: "x", Spec: Spec("n")}}
	RequireErr(t, s.Networks().Create(t.Context(), again), repository.ErrAlreadyExists)

	got, err := s.Networks().GetByCreateRequest(t.Context(), "acme", key)
	if err != nil || got.Name != "a" {
		t.Fatalf("GetByCreateRequest = %v, %v; want network a", got, err)
	}
	MustNetwork(t, s, "acme", "c") // no request ID: NULLs never collide
	MustNetwork(t, s, "acme", "d")
}

func testParentsMustExist(t *testing.T, b Backend) {
	s := b.New(t)
	n := &model.Network{TenantID: "nobody", Name: "x", Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "x", Spec: Spec("n")}}
	RequireErr(t, s.Networks().Create(t.Context(), n), repository.ErrNotFound)

	MustTenant(t, s, "acme")
	c := &model.Cluster{TenantID: "acme", Name: "x", NetworkID: "no-such-network", Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "x", Spec: Spec("c")}}
	RequireErr(t, s.Clusters().Create(t.Context(), c), repository.ErrNotFound)
}

func testNetworkInUseCannotBeDeleted(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	n := MustNetwork(t, s, "acme", "shared")
	MustCluster(t, s, "acme", "a", n)
	MustCluster(t, s, "acme", "b", n)

	RequireErr(t, s.Networks().Delete(t.Context(), "acme", n.ID), repository.ErrInUse)

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

func testTenantWithResourcesCannotBeDeleted(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	MustNetwork(t, s, "acme", "n")
	RequireErr(t, s.Tenants().Delete(t.Context(), "acme"), repository.ErrInUse)

	MustTenant(t, s, "empty")
	if err := s.Tenants().PutCloudAccount(t.Context(), &model.CloudAccount{TenantID: "empty", Provider: "aws", AccountID: "444455556666"}); err != nil {
		t.Fatalf("PutCloudAccount: %v", err)
	}
	if err := s.Tenants().Delete(t.Context(), "empty"); err != nil {
		t.Fatalf("delete tenant with only cloud accounts: %v", err)
	}
	_, err := s.Tenants().GetCloudAccount(t.Context(), "empty", model.ProviderAWS, "444455556666")
	RequireErr(t, err, repository.ErrNotFound) // cascaded
}

func testTenantHasManyCloudAccounts(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
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
	RequireErr(t, err, repository.ErrNotFound)
	RequireErr(t, s.Tenants().PutCloudAccount(t.Context(), &model.CloudAccount{TenantID: "nobody", Provider: "aws", AccountID: "444455556666"}), repository.ErrNotFound)
}

func testNetworkMustBeInItsTenantsAccount(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	MustTenant(t, s, "globex")
	globexAccount := MustAccount(t, s, "globex")
	n := &model.Network{TenantID: "acme", Name: "n", Provider: "aws", Region: "us-east-1", CloudAccountID: globexAccount,
		Resource: model.Resource{State: "x", Spec: Spec("n")}}
	RequireErr(t, s.Networks().Create(t.Context(), n), repository.ErrInvalid)

	n.CloudAccountID = "00000000-0000-0000-0000-000000000000"
	RequireErr(t, s.Networks().Create(t.Context(), n), repository.ErrNotFound)
}

func testUpdateChecksETag(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	n := MustNetwork(t, s, "acme", "n")
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
	RequireErr(t, s.Networks().Update(t.Context(), &stale), repository.ErrConflict)

	got, _ := s.Networks().Get(t.Context(), "acme", n.ID)
	if got.DisplayName != "Shared" || got.Generation != 2 {
		t.Fatalf("stored = %+v", got.Resource)
	}

	gone := *n
	gone.ID = "no-such-row"
	RequireErr(t, s.Networks().Update(t.Context(), &gone), repository.ErrNotFound)
}

func testListPagesNetworksOldestFirst(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	MustTenant(t, s, "globex")
	for _, name := range []string{"c", "a", "e", "b", "d"} {
		MustNetwork(t, s, "acme", name)
	}
	MustNetwork(t, s, "globex", "zzz")

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

func testNotFound(t *testing.T, b Backend) {
	s := b.New(t)
	_, err := s.Clusters().Get(t.Context(), "acme", "missing")
	RequireErr(t, err, repository.ErrNotFound)
	_, err = s.Tenants().Get(t.Context(), "missing")
	RequireErr(t, err, repository.ErrNotFound)
}

func testDeletedParentsTakeNoChildren(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	n := MustNetwork(t, s, "acme", "n")
	c := MustCluster(t, s, "acme", "c", n)
	if err := s.Clusters().Delete(t.Context(), "acme", c.ID); err != nil {
		t.Fatalf("delete cluster: %v", err)
	}

	if err := s.Networks().Delete(t.Context(), "acme", n.ID); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	late := &model.Cluster{TenantID: "acme", Name: "late", NetworkID: n.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "x", Spec: Spec("c")}}
	RequireErr(t, s.Clusters().Create(t.Context(), late), repository.ErrNotFound)
}

func testClusterMustMatchItsNetwork(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	MustTenant(t, s, "globex")
	n := MustNetwork(t, s, "acme", "n") // aws

	otherTenant := &model.Cluster{TenantID: "globex", Name: "c", NetworkID: n.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "x", Spec: Spec("c")}}
	RequireErr(t, s.Clusters().Create(t.Context(), otherTenant), repository.ErrInvalid)

	otherCloud := &model.Cluster{TenantID: "acme", Name: "c", NetworkID: n.ID, Provider: "gcp", Region: "us-central1",
		Resource: model.Resource{State: "x", Spec: Spec("c")}}
	RequireErr(t, s.Clusters().Create(t.Context(), otherCloud), repository.ErrInvalid)
}

// Only AWS accounts are kept: the kinds have no other provider.
func testOnlyAWSAccounts(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	accounts := []model.CloudAccount{
		{Provider: model.ProviderAWS, AccountID: "444455556666"},
		{Provider: model.ProviderGCP, AccountID: "acme-prod-1234"},
		{Provider: model.ProviderAzure, AccountID: "0b1f6b2c-3d4e-4f5a-8b6c-7d8e9f0a1b2c", DirectoryID: "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"},
	}
	for _, a := range accounts {
		a.TenantID = "acme"
		err := s.Tenants().PutCloudAccount(t.Context(), &a)
		if a.Provider != model.ProviderAWS {
			RequireErr(t, err, repository.ErrInvalid)
			_, err = s.Tenants().GetCloudAccount(t.Context(), "acme", a.Provider, a.AccountID)
			RequireErr(t, err, repository.ErrNotFound)
			continue
		}
		if err != nil {
			t.Fatalf("PutCloudAccount(%s): %v", a.Provider, err)
		}
		stored, err := s.Tenants().GetCloudAccount(t.Context(), "acme", a.Provider, a.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		n := &model.Network{TenantID: "acme", Name: string(a.Provider), Provider: a.Provider, Region: "r1", CloudAccountID: stored.ID,
			Resource: model.Resource{State: "x", Spec: Spec("n")}}
		if err := s.Networks().Create(t.Context(), n); err != nil {
			t.Fatalf("network on %s: %v", a.Provider, err)
		}
	}
}

func testProviderRulesAreEnforced(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
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
	n := &model.Network{TenantID: "acme", Name: "n", Provider: "openstack", Region: "r1", CloudAccountID: MustAccount(t, s, "acme"),
		Resource: model.Resource{State: "x", Spec: Spec("n")}}
	RequireErr(t, s.Networks().Create(t.Context(), n), repository.ErrInvalid)
}

func testListCloudAccounts(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	second := model.CloudAccount{Provider: model.ProviderAWS, AccountID: "777788889999"}
	for _, a := range []model.CloudAccount{second, {Provider: model.ProviderAWS, AccountID: "444455556666"}} {
		a.TenantID = "acme"
		if err := s.Tenants().PutCloudAccount(t.Context(), &a); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Tenants().ListCloudAccounts(t.Context(), "acme")
	if err != nil || len(got) != 2 || got[0].AccountID != "444455556666" {
		t.Fatalf("ListCloudAccounts = %+v, %v; want ordered by provider, then account ID", got, err)
	}
	if none, _ := s.Tenants().ListCloudAccounts(t.Context(), "nobody"); len(none) != 0 {
		t.Errorf("unknown tenant has accounts: %+v", none)
	}
}

func testTenantDisplayNameDefaultsToID(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
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

func testManagedResourcesByOwnerAndAccount(t *testing.T, b Backend) {
	s := b.New(t)
	MustTenant(t, s, "acme")
	account := MustAccount(t, s, "acme")
	n := MustNetwork(t, s, "acme", "prod")
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

// A recorded status is written only for the row as it was observed: one a
// call changed since keeps what that call stored. The etag never changes.
func testSetStatusOnlyForTheRowItWasObservedFor(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	n := MustNetwork(t, s, "acme", "n")
	c := MustCluster(t, s, "acme", "prod", n)
	observed := c.ETag

	if err := s.Clusters().SetStatus(ctx, "acme", c.ID, observed, model.JSON(`{"state":"CLUSTER_STATE_RUNNING"}`)); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, err := s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil || string(got.Status) == "" || got.ETag != observed {
		t.Fatalf("status %s, etag %s -> %s (%v); want the status stored and the etag unchanged", got.Status, observed, got.ETag, err)
	}
	// A call changes the row (a takeover, an update).
	got.State = "CLUSTER_STATE_UPDATING"
	got.Status = model.JSON(`{"import":{"phase":"IMPORT_PHASE_CONVERGING"}}`)
	if err := s.Clusters().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	RequireErr(t, s.Clusters().SetStatus(ctx, "acme", c.ID, observed, model.JSON(`{"state":"CLUSTER_STATE_IMPORTED"}`)), repository.ErrConflict)
	after, err := s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil || !strings.Contains(string(after.Status), "CONVERGING") {
		t.Errorf("status after a stale write = %s (%v), want what the call stored", after.Status, err)
	}
	RequireErr(t, s.Clusters().SetStatus(ctx, "acme", "0190f1a2-0000-7000-8000-00000000dead", observed, model.JSON(`{}`)), repository.ErrNotFound)
	RequireErr(t, s.Networks().SetStatus(ctx, "acme", n.ID, "stale", model.JSON(`{}`)), repository.ErrConflict)

	// Two creates in one network do not fail each other, whatever each
	// read of the network before.
	for _, name := range []string{"a", "b"} {
		MustCluster(t, s, "acme", name, n)
	}
}

// A VPC is imported once per account and region; again once that network
// is deleted. Created networks have no external ID and never collide.
func testImportedVPCIsUniquePerAccountAndRegion(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	first := MustNetwork(t, s, "acme", "created")
	MustNetwork(t, s, "acme", "created-too")
	imported := func(name string) *model.Network {
		vpc := "vpc-0123"
		return &model.Network{TenantID: "acme", Name: name, Provider: "aws", Region: "us-east-1",
			CloudAccountID: first.CloudAccountID, ExternalID: &vpc,
			Resource: model.Resource{State: "NETWORK_STATE_IMPORTED", Spec: Spec("network")}}
	}
	a := imported("a")
	if err := s.Networks().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	RequireErr(t, s.Networks().Create(ctx, imported("b")), repository.ErrAlreadyExists)
	if err := s.Networks().Delete(ctx, "acme", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Networks().Create(ctx, imported("c")); err != nil {
		t.Errorf("import after the first was deleted: %v", err)
	}
}
