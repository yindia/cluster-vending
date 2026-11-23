package cluster

import (
	"context"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// catalogPass is a pass's catalog change on one cluster (decideCatalog)
// and what it does to the pass's plan: entries whose change waits keep
// what runs.
type catalogPass struct {
	d catalogDecision
	// absent are held entries with nothing on the cluster yet (a
	// definition added to the catalog): left out of the plan.
	absent map[string]bool
	// removed are held entries the catalog no longer renders: their
	// objects are kept.
	removed map[string]bool
	// live are the held entries with Releases on the cluster: their
	// Releases are not written.
	live map[string]bool
}

// catalogChange decides the pass's catalog change of a cluster from the
// plan this pass renders (PROP-01 4.7).
func (s *Reconciler) catalogChange(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec, in component.PlatformInput,
	plan crossplaneaws.PlatformPlan, move *cloudv1.ClusterReleaseMove) (*catalogPass, error) {
	defs, err := s.deps.Platform.Definitions(ctx)
	if err != nil {
		return nil, err
	}
	rendered, removedSeq, requires, err := s.catalogEntries(row, defs, in)
	if err != nil {
		return nil, err
	}
	stored := storedStatus(row)
	gate := s.gateOf(ctx, row, in.Plan.Namespace)
	comps := map[string]*cloudv1.PlatformComponent{}
	for _, c := range stored.GetPlatformComponents() {
		comps[c.GetName()] = c
	}
	pool := storedPoolPhase(stored)
	f := catalogFacts{Prev: stored.GetCatalogChange(), Now: rendered, RemovedSequence: removedSeq, Requires: requires, Retry: row.Retry != nil,
		Order: s.deps.CatalogOrder, At: now(), ClusterID: row.ID,
		Frozen:   func() (bool, string) { return s.frozen(ctx, row, spec) },
		Admitted: func() (bool, string) { return s.codeAdmitted(ctx, row, spec) },
		Ready: func(name string) bool {
			if name == poolEntry {
				return pool == cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY
			}
			return comps[name].GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_READY
		},
		Failed: func(name string) (bool, string) {
			if name == poolEntry {
				return pool == cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED, "the platform pool failed to apply"
			}
			c := comps[name]
			return c.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, c.GetMessage()
		},
		Settled: func(name string) bool {
			switch {
			case name == poolEntry:
				return pool == cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY
			case !gate.readable(ctx):
				return false
			}
			if _, ok := rendered[name]; !ok {
				return len(gate.releasesOf(ctx, name)) == 0
			}
			c, planned := plannedComponent(plan, name)
			if !planned {
				return true // nothing of it is rendered for this cluster
			}
			return gate.settledAt(ctx, plan.ReleaseDigests(c))
		},
	}
	switch {
	case stored.GetUpgrade() != nil:
		f.Busy = "a Kubernetes upgrade is in progress"
	case moving(stored.GetReleaseMove()) || moving(move):
		f.Busy = "a release move is under way"
	}
	p := &catalogPass{d: decideCatalog(f), absent: map[string]bool{}, removed: map[string]bool{}, live: map[string]bool{}}
	for name := range p.d.Hold {
		switch _, ok := rendered[name]; {
		case name == poolEntry:
		case !ok:
			p.removed[name] = true
		case !gate.readable(ctx) || len(gate.releasesOf(ctx, name)) > 0:
			// Unreadable Releases: assume it runs, so it is held, never
			// left out of the plan (which could remove it).
			p.live[name] = true
		default:
			p.absent[name] = true
		}
	}
	return p, nil
}

// catalogEntries are the cluster's catalog entries this pass: each
// platform definition it gets (for its cloud, NaaS or not: a cluster
// without NaaS has nothing of Capsule to change), fingerprinted with its
// placeholder values, and the platform pool. removedSeq are the
// sequences of every definition, for the order of removals, and
// requires their requirements.
func (s *Reconciler) catalogEntries(row *model.Cluster, defs []cloudv1alpha1.ComponentDefinition, in component.PlatformInput) (
	now map[string]catalogEntry, removedSeq map[string]int32, requires map[string][]string, err error) {
	entries, err := component.PlatformEntries(defs)
	if err != nil {
		return nil, nil, nil, err
	}
	entries, _ = component.ForProvider(entries, string(row.Provider), startedPlatform(row))
	now = make(map[string]catalogEntry, len(entries)+1)
	for _, e := range entries {
		now[e.Name] = catalogEntry{Hash: component.CatalogFingerprint(e, in.Vars(e.Name)), Sequence: e.Sequence}
	}
	if pool := s.platformPoolInput(); pool != nil {
		now[poolEntry] = catalogEntry{Hash: component.Fingerprint(pool), Sequence: poolSequence}
	}
	removedSeq, requires = map[string]int32{}, map[string][]string{}
	for i := range defs {
		if d, err := component.DefinitionOf(&defs[i]); err == nil {
			removedSeq[d.Name], requires[d.Name] = d.Sequence, d.Requires
		}
	}
	return now, removedSeq, requires, nil
}

// storedPoolPhase is the platform pool's phase as the last pass recorded it.
func storedPoolPhase(st *cloudv1.ClusterStatus) cloudv1.NodeGroupPhase {
	for _, g := range st.GetNodeGroups() {
		if g.GetPlatformManaged() && g.GetName() == crossplaneaws.PoolPlatform {
			return g.GetPhase()
		}
	}
	return cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_UNSPECIFIED
}

// plan is the pass's plan without the held definitions that are not on
// the cluster yet.
func (p *catalogPass) plan(plan crossplaneaws.PlatformPlan) crossplaneaws.PlatformPlan {
	if p == nil || len(p.absent) == 0 {
		return plan
	}
	plan.Components = slices.DeleteFunc(slices.Clone(plan.Components), func(c crossplaneaws.PlatformComponentPlan) bool { return p.absent[c.Name] })
	return plan
}

// registered is the plan as the Helm gate learns it: without the held
// definitions, whose changes must not hold other Releases back.
func (p *catalogPass) registered(plan crossplaneaws.PlatformPlan) crossplaneaws.PlatformPlan {
	if p == nil || len(p.live) == 0 {
		return plan
	}
	plan.Components = slices.DeleteFunc(slices.Clone(plan.Components), func(c crossplaneaws.PlatformComponentPlan) bool { return p.live[c.Name] })
	return plan
}

// releaseGate wraps the pass's Release gate: a Release of a held
// definition is not written (what runs stays), one it does not have yet
// waits for the catalog change.
func (p *catalogPass) releaseGate(gate *helmGate, inner func(context.Context, *unstructured.Unstructured) sequence.Decision) func(context.Context, *unstructured.Unstructured) sequence.Decision {
	if p == nil || len(p.live) == 0 {
		return inner
	}
	return func(ctx context.Context, release *unstructured.Unstructured) sequence.Decision {
		name := release.GetLabels()[crossplane.LabelComponent]
		if !p.live[name] {
			if inner == nil {
				return sequence.Decision{}
			}
			return inner(ctx, release)
		}
		if slices.Contains(gate.releasesOf(ctx, name), release.GetName()) {
			return sequence.Decision{Skip: true}
		}
		return sequence.Decision{Hold: "the catalog change of " + name + " (" + p.d.Status.GetMessage() + ")"}
	}
}

// inventory keeps the objects of held removed definitions: their Releases
// and IAM objects, and the Usages that order them.
func (p *catalogPass) inventory(ctx context.Context, gate *helmGate, prefix string, inv sequence.Inventory) sequence.Inventory {
	if p == nil || len(p.removed) == 0 {
		return inv
	}
	releases := map[string]bool{}
	var bases []string
	for name := range p.removed {
		for _, r := range gate.releasesOf(ctx, name) {
			releases[r] = true
		}
		bases = append(bases, crossplaneaws.ComponentIAMBase(prefix, name))
	}
	return sequence.Keeping(inv, func(e sequence.Entry) bool {
		if releases[e.Name] {
			return true
		}
		for _, b := range bases {
			if strings.HasPrefix(e.Name, b) {
				return true
			}
		}
		if e.Kind == "Usage" {
			for name := range p.removed {
				if strings.Contains(e.Name, name) {
					return true
				}
			}
		}
		return false
	})
}

// report is the components' status with the catalog change: a held
// definition not on the cluster yet waits for it (PENDING), a held one
// that runs was not written this pass (Unchanged).
func (p *catalogPass) report(components []*cloudv1.PlatformComponent) {
	if p == nil {
		return
	}
	for _, c := range components {
		switch {
		case p.absent[c.GetName()]:
			c.Phase, c.Message = cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING, "waiting for the catalog change: "+p.d.Status.GetMessage()
		case p.live[c.GetName()]:
			c.Unchanged = true
		}
	}
}

// status is the catalog change to record; nil for no pass.
func (p *catalogPass) status() *cloudv1.CatalogChange {
	if p == nil {
		return nil
	}
	return p.d.Status
}

// holdPool reports whether the platform pool's change waits this pass.
func (p *catalogPass) holdPool() bool { return p != nil && p.d.held(poolEntry) }

// CatalogChanging reports whether the row's recorded status has a catalog
// change waiting for its turn or being applied.
func CatalogChanging(row *model.Cluster) bool {
	return changing(&cloudv1.CodeChange{Phase: storedStatus(row).GetCatalogChange().GetPhase()})
}
