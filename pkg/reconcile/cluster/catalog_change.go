package cluster

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/component"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The catalog change of a cluster (PROP-01 4.7, phase 5): what the
// platform catalog renders for the cluster, fingerprinted per platform
// definition (component.CatalogFingerprint) plus one entry for the
// platform pool, against what the cluster has applied
// (CatalogChange.applied). A difference is applied in turn by the
// channel's code rollout, as a code change is, and on the cluster one
// entry at a time; an entry's recorded fingerprint moves only once its
// Helm operation settled.

// Catalog change phases: the code change's.
const (
	catalogPending  = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING
	catalogApplying = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING
	catalogDone     = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE
	catalogHalted   = cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_HALTED
)

// poolEntry is the catalog entry of the platform pool (nodeGroups.platform).
const poolEntry = crossplaneaws.PoolPlatform

// poolSequence orders the platform pool before every definition: the
// components PROP-03 moves onto it need it first.
const poolSequence = int32(-1)

// catalogEntry is one entry as this pass renders it.
type catalogEntry struct {
	// Hash is its fingerprint; component.UnknownFingerprint when it
	// cannot be computed this pass.
	Hash     string
	Sequence int32
}

// catalogFacts are what deciding a pass of a cluster's catalog change
// reads.
type catalogFacts struct {
	// Prev is the catalog change the last pass recorded; nil (or no
	// applied fingerprints) for a cluster no pass fingerprinted yet.
	Prev *cloudv1.CatalogChange
	// Now are the entries this pass renders, by name.
	Now map[string]catalogEntry
	// RemovedSequence are the sequences of entries no longer rendered
	// (a disabled definition still has one), for the removal order.
	RemovedSequence map[string]int32
	// Requires are the definitions' requirements, by name (removed ones
	// included): a dependent is removed before, and installed after, what
	// it requires.
	Requires map[string][]string
	// Settled reports whether the entry runs what this pass renders: for
	// an entry in Now, its Helm releases carry the rendered digest and
	// settled (deployed, Ready, observed); for a removed one, nothing of
	// it is left on the cluster.
	Settled func(name string) bool
	// Failed reports whether the entry's apply failed, and how.
	Failed func(name string) (bool, string)
	// Ready reports whether the entry was installed and Ready when the
	// last pass looked: a change to one that never was (still installing
	// for the first time, or failing for another reason) is no catalog
	// change and is taken as it comes.
	Ready func(name string) bool
	// Busy says why the cluster's components change already (a Kubernetes
	// upgrade, a release move): each is gated by its own rollout, and a
	// catalog change that lands meanwhile goes with it.
	Busy string
	// Frozen and Admitted are asked only for a change that must start.
	Frozen   func() (bool, string)
	Admitted func() (bool, string)
	// Retry: a person asked to try the cluster's failed work again.
	Retry bool
	// Order are the entries the platform applies first, in this order
	// (reconcile.Deps.CatalogOrder).
	Order     []string
	At        time.Time
	ClusterID string
}

// catalogDecision is one pass of a catalog change.
type catalogDecision struct {
	Status *cloudv1.CatalogChange
	// Hold are the entries whose change waits this pass: a definition in
	// Now keeps what runs (its Releases are not written, a new one is not
	// installed), a removed one is not uninstalled, the pool is not
	// applied.
	Hold map[string]bool
	// Current is the entry being applied; "" for none.
	Current string
	// Recover names the entry this pass tries again (its chart installed
	// again now): it failed or did not settle in time.
	Recover string
}

// poolHeld reports whether the platform pool's change waits this pass.
func (d catalogDecision) poolHeld() bool { return d.Hold[poolEntry] }

// hashes are the fingerprints of entries.
func hashes(entries map[string]catalogEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for name, e := range entries {
		out[name] = e.Hash
	}
	return out
}

// decideCatalog decides one pass of a cluster's catalog change. A cluster
// fingerprinted for the first time takes what it renders as applied.
// Then, for each entry whose fingerprint differs from the one applied, or
// that was added or removed: the change waits its turn in the code
// rollout (admitted, not frozen), then applies one entry at a time in
// order (catalogOrder). The entry being applied is watched like a release
// move's component: one that fails or does not settle within
// componentWatch is recovered up to maxMoveRecoveries times, then the
// change halts there, and stays halted until a retry or a new catalog.
func decideCatalog(f catalogFacts) catalogDecision {
	prev := f.Prev
	if len(prev.GetApplied()) == 0 {
		st := proto.CloneOf(prev)
		if st == nil {
			st = &cloudv1.CatalogChange{}
		}
		st.Applied = hashes(f.Now)
		return catalogDecision{Status: st}
	}
	running := prev.GetPhase() == catalogApplying
	current := ""
	if running || prev.GetPhase() == catalogHalted {
		current = prev.GetComponent()
	}
	applied := maps.Clone(prev.GetApplied())
	step := max(prev.GetStep(), 1)
	// The entry being applied is done once it settled, read from a pass
	// after the one that started it.
	if running && current != "" && prev.GetComponentApplied() && f.Settled(current) {
		if e, ok := f.Now[current]; !ok {
			delete(applied, current)
			current, step = "", step+1
		} else if e.Hash != component.UnknownFingerprint {
			applied[current] = e.Hash
			current, step = "", step+1
		}
	}
	absorb(applied, f, current)
	pending := catalogPendingOf(applied, f.Now)
	target := component.Fingerprint(hashes(f.Now))
	if len(pending) == 0 {
		return catalogDecision{Status: catalogFinished(prev, applied, f.Now, f.At)}
	}
	order := catalogOrder(pending, f.Now, f.RemovedSequence, f.Requires, f.Order)
	st := &cloudv1.CatalogChange{Phase: catalogPending, Applied: applied, Pending: order, Target: target,
		Summary: catalogSummary(order, applied, f.Now)}
	d := catalogDecision{Status: st, Hold: map[string]bool{}}
	for _, name := range order {
		d.Hold[name] = true
	}
	if prev.GetPhase() == catalogHalted && !f.Retry && prev.GetTarget() == target {
		st.Phase, st.Message, st.Component = catalogHalted, prev.GetMessage(), prev.GetComponent()
		st.Step, st.Steps, st.StartTime = prev.GetStep(), prev.GetSteps(), prev.GetStartTime()
		return d
	}
	if !running {
		if frozen, why := f.Frozen(); frozen {
			st.Message = why
			return d
		}
		if admitted, why := f.Admitted(); !admitted {
			st.Message = orDefault(why, "waiting its turn in the channel's code rollout")
			return d
		}
		st.StartTime, step = timestamppb.New(f.At), 1
	} else {
		st.StartTime = prev.GetStartTime()
	}
	st.Phase = catalogApplying
	name := order[0]
	st.Component, st.Step = name, count32(int(step))
	st.Steps = count32(int(step) - 1 + len(order))
	st.ComponentStartTime = timestamppb.New(f.At)
	if running && prev.GetComponent() == name && prev.GetComponentStartTime() != nil && !f.Retry {
		st.ComponentStartTime, st.ComponentApplied, st.Recoveries = prev.GetComponentStartTime(), true, prev.GetRecoveries()
	}
	delete(d.Hold, name)
	d.Current = name
	watchCatalogEntry(f, st, &d)
	return d
}

// absorb takes into applied the changes that are no catalog change to
// gate: everything while the cluster's components change already (Busy);
// an entry whose fingerprint was not known yet; an entry that was never
// Ready (its first install is still going on); a removed entry of which
// nothing is left. An unknown fingerprint keeps the one applied. The entry
// being applied (current) is never absorbed.
func absorb(applied map[string]string, f catalogFacts, current string) {
	for _, name := range slices.Sorted(maps.Keys(f.Now)) {
		e := f.Now[name]
		old, had := applied[name]
		if e.Hash == component.UnknownFingerprint || !had || old == e.Hash || name == current {
			continue
		}
		if f.Busy != "" || old == component.UnknownFingerprint || !f.Ready(name) {
			applied[name] = e.Hash
		}
	}
	if f.Busy != "" {
		for name, e := range f.Now {
			if _, had := applied[name]; !had && e.Hash != component.UnknownFingerprint {
				applied[name] = e.Hash
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(applied)) {
		if _, rendered := f.Now[name]; rendered || name == current {
			continue
		}
		if f.Busy != "" || f.Settled(name) {
			delete(applied, name)
		}
	}
}

// catalogPendingOf are the entries whose change is not applied: added,
// removed, or with another known fingerprint.
func catalogPendingOf(applied map[string]string, now map[string]catalogEntry) []string {
	var out []string
	for name, e := range now {
		old, had := applied[name]
		if !had || e.Hash != component.UnknownFingerprint && e.Hash != old {
			out = append(out, name)
		}
	}
	for name := range applied {
		if _, ok := now[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}

// catalogOrder is the order a catalog change applies its entries in: the
// ones explicit names first, in that order; then removals, highest
// sequence first, a dependent before what it requires; then additions and
// changes, lowest sequence first, what is required before its dependent.
// Ties by name. requires are the definitions' requirements, by name.
func catalogOrder(pending []string, now map[string]catalogEntry, removedSequence map[string]int32, requires map[string][]string, explicit []string) []string {
	var removals, others []string
	for _, name := range pending {
		if _, ok := now[name]; ok {
			others = append(others, name)
		} else {
			removals = append(removals, name)
		}
	}
	seq := func(name string) int32 {
		if e, ok := now[name]; ok {
			return e.Sequence
		}
		return removedSequence[name]
	}
	// A removal goes once no other removal left requires it, the highest
	// sequence of those first.
	removals = takeInOrder(removals, func(name string, left []string) bool {
		return !slices.ContainsFunc(left, func(o string) bool { return o != name && slices.Contains(requires[o], name) })
	}, func(a, b string) int { return cmp.Or(cmp.Compare(seq(b), seq(a)), strings.Compare(a, b)) })
	// The others go once nothing they require is left, the lowest
	// sequence of those first.
	others = takeInOrder(others, func(name string, left []string) bool {
		return !slices.ContainsFunc(requires[name], func(r string) bool { return r != name && slices.Contains(left, r) })
	}, func(a, b string) int { return cmp.Or(cmp.Compare(seq(a), seq(b)), strings.Compare(a, b)) })
	rest := slices.Concat(removals, others)
	out := make([]string, 0, len(rest))
	for _, name := range explicit {
		if slices.Contains(rest, name) && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for _, name := range rest {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// takeInOrder takes names one at a time: the first by less of those ready to
// go given the ones left; when none is (a cycle, which the catalog's
// checks refuse), the first by less.
func takeInOrder(names []string, ready func(name string, left []string) bool, less func(a, b string) int) []string {
	left := slices.Clone(names)
	out := make([]string, 0, len(names))
	for len(left) > 0 {
		slices.SortFunc(left, less)
		i := slices.IndexFunc(left, func(n string) bool { return ready(n, left) })
		if i < 0 {
			i = 0
		}
		out = append(out, left[i])
		left = slices.Delete(left, i, i+1)
	}
	return out
}

// catalogFinished is the change once nothing is pending: done, when one
// was waiting or being applied; else as it was, with what is applied.
func catalogFinished(prev *cloudv1.CatalogChange, applied map[string]string, now map[string]catalogEntry, at time.Time) *cloudv1.CatalogChange {
	st := proto.CloneOf(prev)
	st.Applied, st.Target = applied, component.Fingerprint(hashes(now))
	switch prev.GetPhase() {
	case catalogPending, catalogApplying, catalogHalted:
		st.Phase, st.FinishTime, st.Message, st.Component, st.Pending = catalogDone, timestamppb.New(at), "", "", nil
		st.Step, st.ComponentStartTime, st.ComponentApplied, st.Recoveries = st.GetSteps(), nil, false, 0
	case cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_UNSPECIFIED, cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE:
		// Nothing was waiting: kept as it was, with what is applied.
	}
	return st
}

// catalogSummary says what a change does, for people.
func catalogSummary(ordered []string, applied map[string]string, now map[string]catalogEntry) string {
	parts := make([]string, 0, len(ordered))
	for _, name := range ordered {
		_, had := applied[name]
		_, has := now[name]
		what := "changed"
		switch {
		case !had:
			what = "added"
		case !has:
			what = "removed"
		}
		parts = append(parts, name+" ("+what+")")
	}
	noun := "entries change"
	if len(ordered) == 1 {
		noun = "entry changes"
	}
	return fmt.Sprintf("%d catalog %s: %s", len(ordered), noun, strings.Join(parts, ", "))
}

// watchCatalogEntry is how the entry being applied is doing: applying,
// recovered (d.Recover) when it failed or did not settle within
// componentWatch, or halted after the last recovery.
func watchCatalogEntry(f catalogFacts, st *cloudv1.CatalogChange, d *catalogDecision) {
	name := st.GetComponent()
	since := f.At.Sub(st.GetComponentStartTime().AsTime())
	failed, how := f.Failed(name)
	failed = failed && st.GetComponentApplied() && (st.GetRecoveries() == 0 || since > recoveryGrace)
	stalled := st.GetComponentApplied() && since > componentWatch
	why := fmt.Sprintf("did not settle within %s", componentWatch)
	if failed {
		why = "failed: " + oneLine(how)
	}
	switch {
	case (failed || stalled) && st.GetRecoveries() < maxMoveRecoveries:
		st.Recoveries++
		st.ComponentStartTime = timestamppb.New(f.At) // the watch starts over
		d.Recover = name
		st.Message = fmt.Sprintf("recovering %s (attempt %d of %d): it %s", name, st.GetRecoveries(), maxMoveRecoveries, why)
	case failed || stalled:
		st.Phase = catalogHalted
		st.Message = fmt.Sprintf("%s %s after %d recoveries; cluster retry --id %s resumes the catalog change", name, why, maxMoveRecoveries, f.ClusterID)
		d.Hold[name], d.Current = true, ""
	default:
		st.Message = "applying " + name
	}
}
