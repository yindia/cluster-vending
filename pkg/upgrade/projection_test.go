package upgrade

import (
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

// The 3-tier fixture of the plan view: tier 2 done, tier 1 rolling batch 2
// of 3 with one cluster waiting on its own gate, tier 0 not started.
func rollingFixture() (Policy, []Member) {
	p := Policy{MaxInProgress: 2, MaxFailed: 1, Soak: 24 * time.Hour, SoakPerTier: map[Tier]time.Duration{Tier1: 48 * time.Hour}}
	return p, []Member{
		{ID: "acme/dev", Tier: Tier2, State: Succeeded, FinishedAt: t0.Add(-50 * time.Hour)},
		{ID: "acme/qa", Tier: Tier2, State: Succeeded, FinishedAt: t0.Add(-49 * time.Hour)},
		{ID: "acme/a", Tier: Tier1, State: Succeeded, FinishedAt: t0.Add(-2 * time.Hour)},
		{ID: "acme/b", Tier: Tier1, State: Succeeded, FinishedAt: t0.Add(-time.Hour)},
		{ID: "acme/payments", Tier: Tier1, State: InProgress},
		{ID: "globex/api", Tier: Tier1, State: Pending},
		{ID: "acme/search", Tier: Tier1, State: Pending},
		{ID: "globex/core", Tier: Tier1, State: Pending},
		{ID: "acme/batch", Tier: Tier1, State: Blocked},
		{ID: "acme/prod", Tier: Tier0, State: Pending},
		{ID: "globex/prod", Tier: Tier0, State: Pending},
	}
}

func TestProjectRolling(t *testing.T) {
	p, members := rollingFixture()
	d := Plan(p, t0, members)

	got := Project(p, d, members)

	if !got.Rolling || got.Tier != Tier1 || got.Batch != 2 || got.Batches != 3 || got.Done || got.Halted {
		t.Fatalf("header = %+v, want rolling tier 1, batch 2 of 3", got)
	}
	if len(got.Tiers) != 3 {
		t.Fatalf("tiers = %d, want 3", len(got.Tiers))
	}
	t2, t1, t0p := got.Tiers[0], got.Tiers[1], got.Tiers[2]
	if t2.Tier != Tier2 || t2.State != TierDone || t2.Done != 2 || t2.Total != 2 ||
		!t2.FinishedAt.Equal(t0.Add(-49*time.Hour)) || !t2.SoakUntil.Equal(t0.Add(-25*time.Hour)) {
		t.Errorf("tier 2 = %+v", t2)
	}
	wantBatches := [][]string{{"acme/a", "acme/b"}, {"acme/payments", "acme/search"}, {"globex/api", "globex/core"}}
	if t1.State != TierRolling || t1.BatchSize != 2 || t1.Current != 1 || !equalBatches(t1.Batches, wantBatches) {
		t.Errorf("tier 1 = %+v, want batches %v, current 1", t1, wantBatches)
	}
	if !slices.Equal(t1.Waits, []string{"acme/batch"}) || t1.Total != 7 || t1.MaxFailed != 1 {
		t.Errorf("tier 1 waits %v, total %d", t1.Waits, t1.Total)
	}
	if t0p.State != TierNotStarted || t0p.BatchSize != 1 || !equalBatches(t0p.Batches, [][]string{{"acme/prod"}, {"globex/prod"}}) {
		t.Errorf("tier 0 = %+v", t0p)
	}
}

// The batch that starts next is what Plan starts: the projection and the
// controller agree.
func TestProjectAgreesWithPlan(t *testing.T) {
	p, members := rollingFixture()
	d := Plan(p, t0, members)

	got := Project(p, d, members)

	tier := got.Tiers[1]
	var pending []string
	for _, id := range tier.Batches[tier.Current].Members {
		if stateOf(members, id) == Pending {
			pending = append(pending, id)
		}
	}
	if len(d.Start) == 0 || !slices.Equal(pending, d.Start) {
		t.Errorf("current batch's pending = %v, Plan starts %v", pending, d.Start)
	}
}

func TestProjectHalted(t *testing.T) {
	p := Policy{MaxInProgress: 2, MaxFailed: 1}
	members := []Member{
		{ID: "a", Tier: Tier2, State: Failed, FinishedAt: t0},
		{ID: "b", Tier: Tier2, State: Failed, FinishedAt: t0},
		{ID: "c", Tier: Tier1, State: Pending},
	}
	d := Plan(p, t0, members)

	got := Project(p, d, members)

	if !got.Halted || got.Tiers[0].State != TierHalted || got.Tiers[0].Failed != 2 || got.Tiers[1].State != TierNotStarted {
		t.Errorf("projection = %+v", got)
	}
}

func TestProjectDone(t *testing.T) {
	p := Policy{MaxInProgress: 2, MaxFailed: 1}
	members := []Member{{ID: "a", Tier: Tier2, State: Succeeded, FinishedAt: t0}}
	d := Plan(p, t0, members)

	got := Project(p, d, members)

	if !got.Done || got.Rolling || got.Tiers[0].State != TierDone {
		t.Errorf("projection = %+v", got)
	}
	if empty := Project(p, Plan(p, t0, nil), nil); !empty.Done || empty.Rolling || len(empty.Tiers) != 0 {
		t.Errorf("no members: %+v", empty)
	}
}

// A soak between tiers: the next tier is listed as soaking, not rolling.
func TestProjectSoaking(t *testing.T) {
	p := Policy{MaxInProgress: 2, Soak: 24 * time.Hour}
	members := []Member{
		{ID: "a", Tier: Tier2, State: Succeeded, FinishedAt: t0.Add(-time.Hour)},
		{ID: "b", Tier: Tier1, State: Pending},
	}
	d := Plan(p, t0, members)

	got := Project(p, d, members)

	if got.Tiers[1].State != TierSoaking || !got.Tiers[0].SoakUntil.Equal(t0.Add(23*time.Hour)) {
		t.Errorf("projection = %+v", got)
	}
}

func equalBatches(got []Batch, want [][]string) bool {
	return slices.EqualFunc(got, want, func(b Batch, w []string) bool { return slices.Equal(b.Members, w) })
}

func stateOf(members []Member, id string) State {
	for _, m := range members {
		if m.ID == id {
			return m.State
		}
	}
	return ""
}
