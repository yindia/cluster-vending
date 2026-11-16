package storetest

import (
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// A deleted row is gone for every read, deleting it again finds nothing,
// and what it held alone (a create request ID) can be used again.
func testDeletedRowsAreGoneAndTheirNamesFree(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	key := "3f2b8c1e-4a5d-4e6f-9a7b-1c2d3e4f5a6b"
	network := func() *model.Network {
		return &model.Network{TenantID: "acme", Name: "prod", Provider: "aws", Region: "us-east-1", CreateRequestID: &key,
			CloudAccountID: MustAccount(t, s, "acme"), Resource: model.Resource{State: "x", Spec: Spec("n")}}
	}
	first := network()
	if err := s.Networks().Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.Networks().Delete(ctx, "acme", first.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := s.Networks().Get(ctx, "acme", first.ID)
	RequireErr(t, err, repository.ErrNotFound)
	RequireErr(t, s.Networks().Delete(ctx, "acme", first.ID), repository.ErrNotFound)
	_, err = s.Networks().GetByCreateRequest(ctx, "acme", key)
	RequireErr(t, err, repository.ErrNotFound)
	RequireErr(t, s.Networks().Delete(ctx, "globex", first.ID), repository.ErrNotFound)

	second := network()
	if err := s.Networks().Create(ctx, second); err != nil {
		t.Fatalf("create with the request ID of a deleted network: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("recreate reused the deleted row")
	}
	if got, err := s.Networks().GetByCreateRequest(ctx, "acme", key); err != nil || got.ID != second.ID {
		t.Errorf("GetByCreateRequest = %v, %v; want the second network", got, err)
	}
}

// Records keep the order they were made in, also across calls; what is
// known of an object later (its cloud ID, when it went away) is stored on
// its record, and a record can be forgotten alone.
func testManagedResourceRecords(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	account := MustAccount(t, s, "acme")
	n := MustNetwork(t, s, "acme", "prod")
	other := MustNetwork(t, s, "acme", "other")
	obj := func(owner, kind, name string) model.ManagedResource {
		return model.ManagedResource{TenantID: "acme", CloudAccountID: account, OwnerKind: model.OwnerNetwork, OwnerID: owner,
			APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: kind, Namespace: "tenant-acme", Name: name}
	}
	carried := obj(n.ID, "FlowLog", "prod-flow-log")
	carried.CarriedOver = true
	for _, batch := range [][]model.ManagedResource{
		{obj(n.ID, "VPC", "prod-vpc"), obj(n.ID, "Subnet", "prod-b"), obj(n.ID, "Subnet", "prod-a")},
		{obj(n.ID, "VPC", "prod-vpc"), obj(n.ID, "NATGateway", "prod-nat"), carried}, // the VPC again: left as it is
		{obj(other.ID, "VPC", "prod-vpc")},                                           // the same object name for another owner
	} {
		if err := s.ManagedResources().Put(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, n.ID)
	if err != nil || len(rows) != 5 {
		t.Fatalf("ListByOwner = %d records (%v), want 5", len(rows), err)
	}
	for i, want := range []string{"prod-vpc", "prod-b", "prod-a", "prod-nat", "prod-flow-log"} {
		if rows[i].Name != want || rows[i].ID == "" {
			t.Errorf("record %d = %s (ID %q), want %s: records list in the order they were made", i, rows[i].Name, rows[i].ID, want)
		}
	}
	if !rows[4].CarriedOver || rows[0].CarriedOver {
		t.Error("the carried-over mark was not stored")
	}

	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := s.ManagedResources().SetExternalName(ctx, rows[0].ID, "vpc-0abc"); err != nil {
		t.Fatal(err)
	}
	for _, when := range []time.Time{at, at.Add(time.Hour)} { // a time already stored is kept
		if err := s.ManagedResources().SetGoneAt(ctx, rows[1].ID, when); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ManagedResources().Delete(ctx, rows[2].ID); err != nil {
		t.Fatal(err)
	}
	RequireErr(t, s.ManagedResources().Delete(ctx, rows[2].ID), repository.ErrNotFound)
	if err := s.ManagedResources().SetExternalName(ctx, rows[2].ID, "gone"); err != nil {
		t.Errorf("SetExternalName of a forgotten record: %v", err)
	}

	byOwner, err := s.ManagedResources().ListByOwners(ctx, model.OwnerNetwork, []string{n.ID, other.ID, "nobody"})
	if err != nil || len(byOwner[n.ID]) != 4 || len(byOwner[other.ID]) != 1 || len(byOwner["nobody"]) != 0 {
		t.Fatalf("ListByOwners = %d and %d records (%v), want 4 and 1", len(byOwner[n.ID]), len(byOwner[other.ID]), err)
	}
	got := byOwner[n.ID]
	if got[0].ExternalName != "vpc-0abc" || got[1].GoneAt == nil || !got[1].GoneAt.Equal(at) || got[2].Name != "prod-nat" {
		t.Errorf("records after the changes = %+v", got)
	}
	if none, err := s.ManagedResources().ListByOwners(ctx, model.OwnerNetwork, nil); err != nil || len(none) != 0 {
		t.Errorf("ListByOwners of no owner = %v (%v)", none, err)
	}
}

// Clusters are found by their create request ID, their name and their
// network, each within what it is unique in.
func testClusterLookups(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	MustTenant(t, s, "globex")
	n := MustNetwork(t, s, "acme", "n")
	key := "7d1a3b5c-2e4f-4a6b-8c9d-0e1f2a3b4c5d"
	c := &model.Cluster{TenantID: "acme", Name: "prod", NetworkID: n.ID, Provider: "aws", Region: "us-east-1", CreateRequestID: &key,
		Resource: model.Resource{State: "CLUSTER_STATE_PROVISIONING", Spec: Spec("cluster"), Labels: model.Labels{"team": "a"}},
		Input:    Spec("input"), Adoption: Spec("adoption"), DeleteForce: true, ResolvedGeneration: 1}
	if err := s.Clusters().Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	MustCluster(t, s, "acme", "staging", n)
	MustCluster(t, s, "globex", "prod", networkInOtherAccount(t, s, "globex", "n"))

	got, err := s.Clusters().GetByCreateRequest(ctx, "acme", key)
	if err != nil || got.ID != c.ID || got.Labels["team"] != "a" || len(got.Input) == 0 || len(got.Adoption) == 0 || !got.DeleteForce || got.ResolvedGeneration != 1 ||
		got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("GetByCreateRequest = %+v, %v", got, err)
	}
	_, err = s.Clusters().GetByCreateRequest(ctx, "globex", key)
	RequireErr(t, err, repository.ErrNotFound)
	again := &model.Cluster{TenantID: "acme", Name: "other", NetworkID: n.ID, Provider: "aws", Region: "us-east-1", CreateRequestID: &key,
		Resource: model.Resource{State: "x", Spec: Spec("cluster")}}
	RequireErr(t, s.Clusters().Create(ctx, again), repository.ErrAlreadyExists)
	if rows, err := s.Clusters().ListByNetwork(ctx, n.ID); err != nil || len(rows) != 2 {
		t.Errorf("a refused create stored a cluster: %d in the network (%v)", len(rows), err)
	}

	named, err := s.Clusters().ListByName(ctx, "acme", "prod")
	if err != nil || len(named) != 1 || named[0].ID != c.ID {
		t.Errorf("ListByName = %+v, %v; want the tenant's own prod", named, err)
	}
	page, err := s.Clusters().List(ctx, "acme", repository.Page{Limit: 1, After: c.ID})
	if err != nil || len(page) != 1 || page[0].Name != "staging" {
		t.Errorf("List after the first = %+v, %v", page, err)
	}
	_, err = s.Clusters().Get(ctx, "globex", c.ID)
	RequireErr(t, err, repository.ErrNotFound)
	RequireErr(t, s.Clusters().Delete(ctx, "globex", c.ID), repository.ErrNotFound)
}
