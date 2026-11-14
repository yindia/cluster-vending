package storetest

import (
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// pausedRows reads who paused each row of tenant acme: "" for a row that
// is not paused.
func pausedRows(t *testing.T, s repository.Store, networks []*model.Network, clusters []*model.Cluster) map[string]string {
	const tenant = "acme"
	t.Helper()
	by := func(p *model.Pause) string {
		if p == nil {
			return ""
		}
		return p.By
	}
	out := map[string]string{}
	accounts, err := s.Tenants().ListCloudAccounts(t.Context(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		out["account"] = by(a.Paused)
	}
	for _, n := range networks {
		row, err := s.Networks().Get(t.Context(), tenant, n.ID)
		if err != nil {
			t.Fatal(err)
		}
		out[row.Name] = by(row.Paused)
	}
	for _, c := range clusters {
		row, err := s.Clusters().Get(t.Context(), tenant, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		out[row.Name] = by(row.Paused)
	}
	return out
}

func requirePaused(t *testing.T, got, want map[string]string) {
	t.Helper()
	for name, by := range want {
		if got[name] != by {
			t.Errorf("%s paused by %q, want %q (all: %v)", name, got[name], by, got)
		}
	}
}

// A pause is recorded on the tenant's rows, kept across their updates,
// inherited by rows created while the tenant is paused, and removed by a
// resume: of the tenant for all of them, of one cluster for that one.
func testPauseIsRecordedInheritedAndRemoved(t *testing.T, b Backend) {
	ctx := t.Context()
	s := b.New(t)
	MustTenant(t, s, "acme")
	MustTenant(t, s, "globex")
	network := MustNetwork(t, s, "acme", "net")
	prod := MustCluster(t, s, "acme", "prod", network)
	other := MustCluster(t, s, "globex", "prod", MustNetwork(t, s, "globex", "net"))
	since := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	pause := &model.Pause{By: "alice", Since: since, Reason: "provider upgrade"}

	// One cluster.
	if err := s.Tenants().SetPause(ctx, "acme", prod.ID, pause); err != nil {
		t.Fatal(err)
	}
	requirePaused(t, pausedRows(t, s, []*model.Network{network}, []*model.Cluster{prod}),
		map[string]string{"account": "", "net": "", "prod": "alice"})
	if err := s.Tenants().SetPause(ctx, "acme", prod.ID, nil); err != nil {
		t.Fatal(err)
	}

	// The tenant: everything it has, and nothing of another tenant.
	if err := s.Tenants().SetPause(ctx, "acme", "", pause); err != nil {
		t.Fatal(err)
	}
	requirePaused(t, pausedRows(t, s, []*model.Network{network}, []*model.Cluster{prod}),
		map[string]string{"account": "alice", "net": "alice", "prod": "alice"})
	if row, err := s.Clusters().Get(ctx, "globex", other.ID); err != nil || row.Paused != nil {
		t.Fatalf("another tenant's cluster: paused %+v, %v", row.Paused, err)
	}
	row, err := s.Clusters().Get(ctx, "acme", prod.ID)
	if err != nil || row.Paused == nil || !row.Paused.Since.Equal(since) || row.Paused.Reason != "provider upgrade" {
		t.Fatalf("pause read back as %+v, %v", row.Paused, err)
	}

	// Pausing again keeps who paused first and when.
	if err := s.Tenants().SetPause(ctx, "acme", "", &model.Pause{By: "bob", Since: since.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	requirePaused(t, pausedRows(t, s, []*model.Network{network}, []*model.Cluster{prod}),
		map[string]string{"account": "alice", "net": "alice", "prod": "alice"})

	// An update of the row keeps the pause.
	row.DisplayName = "Production"
	if err := s.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	// A row created while the tenant is paused is paused.
	late := MustCluster(t, s, "acme", "late", network)
	lateNet := MustNetwork(t, s, "acme", "late-net")
	requirePaused(t, pausedRows(t, s, []*model.Network{network, lateNet}, []*model.Cluster{prod, late}),
		map[string]string{"prod": "alice", "late": "alice", "late-net": "alice"})

	// Resuming one cluster leaves the rest of the tenant paused.
	if err := s.Tenants().SetPause(ctx, "acme", prod.ID, nil); err != nil {
		t.Fatal(err)
	}
	requirePaused(t, pausedRows(t, s, []*model.Network{network}, []*model.Cluster{prod, late}),
		map[string]string{"account": "alice", "net": "alice", "prod": "", "late": "alice"})

	// Resuming the tenant resumes everything, and new rows are not paused.
	if err := s.Tenants().SetPause(ctx, "acme", "", nil); err != nil {
		t.Fatal(err)
	}
	after := MustCluster(t, s, "acme", "after", network)
	requirePaused(t, pausedRows(t, s, []*model.Network{network, lateNet}, []*model.Cluster{prod, late, after}),
		map[string]string{"account": "", "net": "", "late-net": "", "prod": "", "late": "", "after": ""})

	RequireErr(t, s.Tenants().SetPause(ctx, "nobody", "", pause), repository.ErrNotFound)
	RequireErr(t, s.Tenants().SetPause(ctx, "acme", "01a1d0a6-0000-7000-8000-000000000000", pause), repository.ErrNotFound)
}
