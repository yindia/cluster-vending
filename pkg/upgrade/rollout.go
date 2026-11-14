package upgrade

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Tier is a cluster's sensitivity: Tier0 is the most sensitive and goes
// last.
type Tier int

const (
	Tier0 Tier = 0
	Tier1 Tier = 1
	Tier2 Tier = 2
)

// rolloutOrder is the order tiers are upgraded in.
var rolloutOrder = []Tier{Tier2, Tier1, Tier0}

// State is where a cluster is in its channel's rollout.
type State string

const (
	// Pending: it should move and nothing of its own blocks it.
	Pending State = "Pending"
	// InProgress: its upgrade is running.
	InProgress State = "InProgress"
	// Succeeded: it runs the target.
	Succeeded State = "Succeeded"
	// Failed: its upgrade failed.
	Failed State = "Failed"
	// Blocked: its own gates (Check) block it; it does not hold the
	// rollout back.
	Blocked State = "Blocked"
)

// Policy is a channel's rollout policy (ReleaseChannel spec.policy).
type Policy struct {
	// MaxInProgress clusters upgrade at once per tier; tier 0 always one;
	// below one is one.
	MaxInProgress int
	// MaxFailed failed clusters in a tier are tolerated; one more halts
	// the channel.
	MaxFailed int
	// Soak is how long the rollout waits after a tier finished before the
	// next one starts; SoakPerTier overrides it for the tier that
	// finished.
	Soak        time.Duration
	SoakPerTier map[Tier]time.Duration
}

// Member is one cluster of the channel.
type Member struct {
	ID    string
	Tier  Tier
	State State
	// FinishedAt is when its upgrade succeeded or failed.
	FinishedAt time.Time
}

// TierCount counts a tier's clusters by state.
type TierCount struct {
	Pending, InProgress, Succeeded, Failed, Blocked int
}

// Decision is what a channel's rollout does now.
type Decision struct {
	// Start are the clusters to start upgrading now, in order.
	Start []string
	// Stage is the tier being rolled out (or waited for).
	Stage Tier
	// Waiting says why nothing (more) starts; empty when Start is not.
	Waiting string
	// SoakUntil is when the soak the rollout waits for ends; zero when it
	// waits for none.
	SoakUntil time.Time
	// Halted: too many clusters failed; nothing starts.
	Halted bool
	// Done: no cluster is waiting or upgrading.
	Done bool
	// Tiers counts each tier's clusters, by Tier.
	Tiers [3]TierCount
}

// Plan decides which of a channel's clusters start upgrading at now (the
// package documentation has the rules). It does not change members.
func Plan(p Policy, now time.Time, members []Member) Decision {
	byTier := map[Tier][]Member{}
	var d Decision
	for _, m := range members {
		tier := tierOf(m.Tier)
		byTier[tier] = append(byTier[tier], m)
		count(&d.Tiers[tier], m.State)
	}
	maxFailed := max(p.MaxFailed, 0)
	for _, tier := range rolloutOrder {
		if failed := d.Tiers[tier].Failed; failed > maxFailed {
			d.Stage, d.Halted = tier, true
			d.Waiting = fmt.Sprintf("halted: %s failed in tier %d, at most %d allowed", clusters(failed), tier, maxFailed)
			return d
		}
	}
	var finished *finishedTier
	for _, tier := range rolloutOrder {
		c := d.Tiers[tier]
		if c.Pending == 0 && c.InProgress == 0 {
			if last := lastFinished(byTier[tier]); !last.IsZero() {
				finished = &finishedTier{tier: tier, at: last}
			}
			continue
		}
		d.Stage = tier
		if finished != nil {
			if opens := finished.at.Add(p.soak(finished.tier)); now.Before(opens) {
				d.SoakUntil = opens
				d.Waiting = fmt.Sprintf("tier %d finished; tier %d starts after the soak, at %s",
					finished.tier, tier, opens.UTC().Format("2006-01-02 15:04")+" UTC")
				return d
			}
		}
		d.Start, d.Waiting = start(byTier[tier], tier, c, p.MaxInProgress)
		return d
	}
	d.Stage, d.Done = Tier0, true
	return d
}

type finishedTier struct {
	tier Tier
	at   time.Time
}

// start picks a tier's pending clusters to start, by ID, up to its limit.
func start(members []Member, tier Tier, c TierCount, maxInProgress int) ([]string, string) {
	limit := max(maxInProgress, 1)
	if tier == Tier0 {
		limit = 1
	}
	free := limit - c.InProgress
	if c.Pending == 0 {
		return nil, fmt.Sprintf("tier %d: waiting for %s to finish", tier, clusters(c.InProgress))
	}
	if free <= 0 {
		return nil, fmt.Sprintf("tier %d: %d upgrading, at most %d at once", tier, c.InProgress, limit)
	}
	pending := make([]string, 0, c.Pending)
	for _, m := range members {
		if m.State == Pending {
			pending = append(pending, m.ID)
		}
	}
	slices.SortFunc(pending, strings.Compare)
	return pending[:min(free, len(pending))], ""
}

func (p Policy) soak(tier Tier) time.Duration {
	if d, ok := p.SoakPerTier[tier]; ok {
		return max(d, 0)
	}
	return max(p.Soak, 0)
}

// tierOf is the tier a cluster is rolled out in: an unknown tier is the
// most careful one.
func tierOf(t Tier) Tier {
	if t == Tier1 || t == Tier2 {
		return t
	}
	return Tier0
}

func count(c *TierCount, s State) {
	switch s {
	case Pending:
		c.Pending++
	case InProgress:
		c.InProgress++
	case Succeeded:
		c.Succeeded++
	case Failed:
		c.Failed++
	default:
		c.Blocked++
	}
}

// lastFinished is when a tier's last upgrade ended; zero when none did.
func lastFinished(members []Member) time.Time {
	var last time.Time
	for _, m := range members {
		if (m.State == Succeeded || m.State == Failed) && m.FinishedAt.After(last) {
			last = m.FinishedAt
		}
	}
	return last
}

func clusters(n int) string {
	if n == 1 {
		return "1 cluster"
	}
	return fmt.Sprintf("%d clusters", n)
}
