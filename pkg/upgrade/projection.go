package upgrade

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

// TierState is where a tier is in its channel's rollout.
type TierState string

const (
	// TierDone: none of its clusters waits or upgrades.
	TierDone TierState = "Done"
	// TierRolling: its clusters are being upgraded, a batch at a time.
	TierRolling TierState = "Rolling"
	// TierSoaking: it is next, and waits for the tier before it to soak.
	TierSoaking TierState = "Soaking"
	// TierNotStarted: a tier before it is not done yet (or halted).
	TierNotStarted TierState = "NotStarted"
	// TierHalted: more clusters failed than the policy allows.
	TierHalted TierState = "Halted"
)

// Batch is clusters upgraded together, at most the tier's batch size.
type Batch struct{ Members []string }

// TierPlan is one tier of a projection.
type TierPlan struct {
	Tier  Tier
	State TierState
	// Total counts its clusters; Done, InProgress, Failed and Blocked by
	// state; MaxFailed is the failures it may have before the channel
	// halts.
	Total, Done, InProgress, Failed, Blocked, MaxFailed int
	// BatchSize is how many of its clusters upgrade at once (tier 0: one).
	BatchSize int
	// FinishedAt is when its last upgrade ended; SoakUntil when the next
	// tier may start (FinishedAt plus the tier's soak). Zero while none
	// ended.
	FinishedAt, SoakUntil time.Time
	// Batches are its clusters in the order they are (or were) upgraded:
	// the finished ones by when they ended, then the one upgrading and the
	// ones queued, by ID (the order Plan starts them in).
	Batches []Batch
	// Current is the index in Batches of the batch upgrading or next; -1
	// when every batch is finished.
	Current int
	// Waits are its clusters blocked by their own gate, by ID: in no batch;
	// one joins the batch being filled when its gate clears.
	Waits []string
}

// Projection is how a channel's rollout proceeds from now: Plan's
// decision, laid out per tier and batch.
type Projection struct {
	// Tiers are the tiers that have clusters, in rollout order.
	Tiers []TierPlan
	// Rolling: Tier is the tier rolling (or soaking toward); Batch of
	// Batches the batch in it (1-based).
	Rolling        bool
	Tier           Tier
	Batch, Batches int
	Done, Halted   bool
}

// Project lays out d, the decision Plan made for members under p, per tier
// and batch. The batch Current of the rolling tier holds exactly the
// clusters Plan starts (d.Start) besides those upgrading. Batches of
// finished clusters are reconstructed by finish time: a projection, not a
// record of the batches as they ran.
func Project(p Policy, d Decision, members []Member) Projection {
	out := Projection{Done: d.Done, Halted: d.Halted}
	byTier := map[Tier][]Member{}
	for _, m := range members {
		byTier[tierOf(m.Tier)] = append(byTier[tierOf(m.Tier)], m)
	}
	reached := false // a tier at or after the one rolling
	for _, tier := range rolloutOrder {
		ms := byTier[tier]
		if len(ms) == 0 {
			if tier == d.Tier && !d.Done {
				reached = true
			}
			continue
		}
		tp := planTier(p, tier, ms)
		switch {
		case d.Halted && tier == d.Tier:
			tp.State, reached = TierHalted, true
		case d.Done || (!reached && tier != d.Tier):
			tp.State = TierDone
		case tier == d.Tier:
			tp.State, reached = TierRolling, true
			if !d.SoakUntil.IsZero() {
				tp.State = TierSoaking
			}
			out.Rolling, out.Tier = true, tier
			out.Batches = len(tp.Batches)
			out.Batch = tp.Current + 1
		default:
			tp.State = TierNotStarted
		}
		out.Tiers = append(out.Tiers, tp)
	}
	return out
}

// planTier lays one tier's clusters out in batches.
func planTier(p Policy, tier Tier, ms []Member) TierPlan {
	tp := TierPlan{Tier: tier, Total: len(ms), MaxFailed: max(p.MaxFailed, 0), BatchSize: max(p.MaxInProgress, 1), Current: -1}
	if tier == Tier0 {
		tp.BatchSize = 1
	}
	var finished, inProgress, pending []Member
	for _, m := range ms {
		switch m.State {
		case Succeeded:
			tp.Done++
			finished = append(finished, m)
		case Failed:
			tp.Failed++
			finished = append(finished, m)
		case InProgress:
			tp.InProgress++
			inProgress = append(inProgress, m)
		case Pending:
			pending = append(pending, m)
		default:
			tp.Blocked++
			tp.Waits = append(tp.Waits, m.ID)
		}
	}
	slices.SortFunc(finished, func(a, b Member) int {
		return cmp.Or(a.FinishedAt.Compare(b.FinishedAt), strings.Compare(a.ID, b.ID))
	})
	byID := func(a, b Member) int { return strings.Compare(a.ID, b.ID) }
	slices.SortFunc(inProgress, byID)
	slices.SortFunc(pending, byID)
	slices.Sort(tp.Waits)
	active := slices.Concat(inProgress, pending)
	tp.Batches = append(batches(finished, tp.BatchSize), batches(active, tp.BatchSize)...)
	if len(active) > 0 {
		tp.Current = len(tp.Batches) - len(batches(active, tp.BatchSize))
	}
	if last := lastFinished(ms); !last.IsZero() {
		tp.FinishedAt = last
		tp.SoakUntil = last.Add(p.soak(tier))
	}
	return tp
}

// batches splits ms, in order, into batches of size.
func batches(ms []Member, size int) []Batch {
	var out []Batch
	for chunk := range slices.Chunk(ms, size) {
		b := Batch{}
		for _, m := range chunk {
			b.Members = append(b.Members, m.ID)
		}
		out = append(out, b)
	}
	return out
}
