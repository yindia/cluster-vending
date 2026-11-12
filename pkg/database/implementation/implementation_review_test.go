package implementation_test

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// A row marked behind is applied again, and a write based on the row as it
// was fails.
func TestMarkBehind(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	mustTenant(t, s, "acme")
	n := mustNetwork(t, s, "acme", "n")
	c := mustCluster(t, s, "acme", "prod", n)
	c.Generation, c.AppliedGeneration = 3, 3
	if err := s.Clusters().Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	for range 2 { // repeating it does not move the generation further back
		if err := s.Clusters().MarkBehind(ctx, "acme", c.ID); err != nil {
			t.Fatalf("MarkBehind: %v", err)
		}
	}
	got, err := s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil || got.AppliedGeneration != 2 || got.Generation != 3 || got.ETag == c.ETag {
		t.Fatalf("applied %d of %d, etag changed %t (%v); want 2 of 3 and a new etag",
			got.AppliedGeneration, got.Generation, got.ETag != c.ETag, err)
	}
	requireErr(t, s.Clusters().Update(ctx, c), repository.ErrConflict)
	if err := s.Networks().MarkBehind(ctx, "acme", n.ID); err != nil {
		t.Fatalf("MarkBehind network: %v", err)
	}
	requireErr(t, s.Networks().MarkBehind(ctx, "acme", "0190f1a2-0000-7000-8000-00000000dead"), repository.ErrNotFound)
}

// A VPC is imported once per account and region; again once that network
// is deleted. Created networks have no external ID and never collide.
func TestImportedVPCIsUniquePerAccountAndRegion(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	mustTenant(t, s, "acme")
	first := mustNetwork(t, s, "acme", "created")
	mustNetwork(t, s, "acme", "created-too")
	imported := func(name string) *model.Network {
		vpc := "vpc-0123"
		return &model.Network{TenantID: "acme", Name: name, Provider: "aws", Region: "us-east-1",
			CloudAccountID: first.CloudAccountID, ExternalID: &vpc,
			Resource: model.Resource{State: "NETWORK_STATE_IMPORTED", Spec: spec("network")}}
	}
	a := imported("a")
	if err := s.Networks().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	requireErr(t, s.Networks().Create(ctx, imported("b")), repository.ErrAlreadyExists)
	if err := s.Networks().Delete(ctx, "acme", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Networks().Create(ctx, imported("c")); err != nil {
		t.Errorf("import after the first was deleted: %v", err)
	}
}
