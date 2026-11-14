package upgrade

import (
	"reflect"
	"testing"
	"time"
)

func TestPlan(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	policy := Policy{MaxInProgress: 2, MaxFailed: 1, Soak: 24 * time.Hour}
	m := func(id string, tier Tier, state State) Member { return Member{ID: id, Tier: tier, State: state} }
	done := func(id string, tier Tier, ago time.Duration) Member {
		return Member{ID: id, Tier: tier, State: Succeeded, FinishedAt: now.Add(-ago)}
	}
	tests := []struct {
		name    string
		policy  Policy
		members []Member
		want    Decision
	}{
		{name: "no clusters", policy: policy, want: Decision{Tier: Tier0, Done: true}},
		{name: "tier 2 first, two at once, by ID",
			policy: policy,
			members: []Member{m("c", Tier2, Pending), m("a", Tier2, Pending), m("b", Tier2, Pending),
				m("t1", Tier1, Pending), m("t0", Tier0, Pending)},
			want: Decision{Tier: Tier2, Start: []string{"a", "b"}}},
		{name: "a batch is running: the next waits",
			policy:  policy,
			members: []Member{m("a", Tier2, InProgress), m("b", Tier2, InProgress), m("c", Tier2, Pending)},
			want:    Decision{Tier: Tier2, Waiting: "tier 2: 2 upgrading, at most 2 at once"}},
		{name: "one of the batch finished: one more starts",
			policy:  policy,
			members: []Member{done("a", Tier2, time.Minute), m("b", Tier2, InProgress), m("c", Tier2, Pending), m("d", Tier2, Pending)},
			want:    Decision{Tier: Tier2, Start: []string{"c"}}},
		{name: "tier 2 done: tier 1 waits for the soak",
			policy:  policy,
			members: []Member{done("a", Tier2, 2*time.Hour), done("b", Tier2, time.Hour), m("t1", Tier1, Pending)},
			want: Decision{Tier: Tier1, SoakUntil: time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC),
				Waiting: "tier 2 finished; tier 1 starts after the soak, at 2026-11-16 11:00 UTC"}},
		{name: "soak passed: tier 1 starts",
			policy:  policy,
			members: []Member{done("a", Tier2, 25*time.Hour), m("t1", Tier1, Pending), m("t0", Tier0, Pending)},
			want:    Decision{Tier: Tier1, Start: []string{"t1"}}},
		{name: "soak per tier overrides the soak",
			policy:  Policy{MaxInProgress: 2, MaxFailed: 1, Soak: 24 * time.Hour, SoakPerTier: map[Tier]time.Duration{Tier2: time.Hour}},
			members: []Member{done("a", Tier2, 2*time.Hour), m("t1", Tier1, Pending)},
			want:    Decision{Tier: Tier1, Start: []string{"t1"}}},
		{name: "tier 0 one at a time",
			policy: policy,
			members: []Member{done("a", Tier2, 72*time.Hour), done("t1", Tier1, 48*time.Hour),
				m("z", Tier0, Pending), m("y", Tier0, Pending)},
			want: Decision{Tier: Tier0, Start: []string{"y"}}},
		{name: "tier 0 running: the next waits",
			policy:  policy,
			members: []Member{m("y", Tier0, InProgress), m("z", Tier0, Pending)},
			want:    Decision{Tier: Tier0, Waiting: "tier 0: 1 upgrading, at most 1 at once"}},
		{name: "one failure is allowed",
			policy:  policy,
			members: []Member{m("a", Tier2, Failed), m("b", Tier2, Pending)},
			want:    Decision{Tier: Tier2, Start: []string{"b"}}},
		{name: "more failures than allowed halt the channel",
			policy:  policy,
			members: []Member{m("a", Tier2, Failed), m("b", Tier2, Failed), m("c", Tier2, Pending), m("t1", Tier1, Pending)},
			want: Decision{Tier: Tier2, Halted: true,
				Waiting: "halted: 2 clusters failed in tier 2, at most 1 allowed"}},
		{name: "no failure allowed",
			policy:  Policy{MaxInProgress: 2, MaxFailed: 0, Soak: time.Hour},
			members: []Member{m("a", Tier2, Failed), m("b", Tier2, Pending)},
			want:    Decision{Tier: Tier2, Halted: true, Waiting: "halted: 1 cluster failed in tier 2, at most 0 allowed"}},
		{name: "a blocked cluster does not hold its tier",
			policy:  policy,
			members: []Member{m("a", Tier2, Blocked), done("b", Tier2, 48*time.Hour), m("t1", Tier1, Pending)},
			want:    Decision{Tier: Tier1, Start: []string{"t1"}}},
		{name: "a tier with only blocked clusters is skipped without a soak",
			policy:  policy,
			members: []Member{m("a", Tier2, Blocked), m("t1", Tier1, Pending)},
			want:    Decision{Tier: Tier1, Start: []string{"t1"}}},
		{name: "a blocked cluster in an earlier tier starts when its gate clears",
			policy:  policy,
			members: []Member{m("a", Tier2, Pending), m("t1", Tier1, InProgress)},
			want:    Decision{Tier: Tier2, Start: []string{"a"}}},
		{name: "everything done",
			policy:  policy,
			members: []Member{done("a", Tier2, time.Hour), done("t0", Tier0, time.Minute), m("x", Tier1, Blocked)},
			want:    Decision{Tier: Tier0, Done: true}},
		{name: "zero policy: one at a time, no failure, no soak",
			policy:  Policy{},
			members: []Member{m("a", Tier2, Pending), m("b", Tier2, Pending)},
			want:    Decision{Tier: Tier2, Start: []string{"a"}}},
		{name: "an unknown tier is treated as tier 0",
			policy:  policy,
			members: []Member{m("x", Tier(7), Pending), m("y", Tier(-1), Pending)},
			want:    Decision{Tier: Tier0, Start: []string{"x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Plan(tt.policy, now, tt.members)
			got.Tiers = [3]TierCount{} // counted in TestPlanCounts
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Plan() =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestPlanCounts(t *testing.T) {
	members := []Member{
		{ID: "a", Tier: Tier2, State: Succeeded}, {ID: "b", Tier: Tier2, State: InProgress},
		{ID: "c", Tier: Tier2, State: Failed}, {ID: "d", Tier: Tier1, State: Pending},
		{ID: "e", Tier: Tier1, State: Blocked}, {ID: "f", Tier: Tier0, State: Pending},
	}
	got := Plan(Policy{MaxInProgress: 1, MaxFailed: 1}, time.Now(), members).Tiers
	want := [3]TierCount{
		Tier0: {Pending: 1},
		Tier1: {Pending: 1, Blocked: 1},
		Tier2: {Succeeded: 1, InProgress: 1, Failed: 1},
	}
	if got != want {
		t.Fatalf("Tiers = %+v, want %+v", got, want)
	}
}

func TestPlanDoesNotChangeItsInput(t *testing.T) {
	members := []Member{{ID: "b", Tier: Tier2, State: Pending}, {ID: "a", Tier: Tier2, State: Pending}}
	Plan(Policy{MaxInProgress: 2}, time.Now(), members)
	if members[0].ID != "b" {
		t.Fatalf("Plan sorted its input: %+v", members)
	}
}
