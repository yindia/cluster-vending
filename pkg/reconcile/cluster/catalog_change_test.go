package cluster

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/component"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var catalogAt = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// catalogWorld is the facts of a cluster with base (100), mesh (200,
// requires base) and policy (300) applied and Ready; each test changes
// what it renders or how it stands.
func catalogWorld() catalogFacts {
	return catalogFacts{
		Prev: &cloudv1.CatalogChange{Applied: map[string]string{"base": "b1", "mesh": "m1", "policy": "p1"}},
		Now: map[string]catalogEntry{"base": {Hash: "b1", Sequence: 100}, "mesh": {Hash: "m1", Sequence: 200},
			"policy": {Hash: "p1", Sequence: 300}},
		RemovedSequence: map[string]int32{},
		Requires:        map[string][]string{"mesh": {"base"}},
		Settled:         func(string) bool { return false },
		Failed:          func(string) (bool, string) { return false, "" },
		Ready:           func(string) bool { return true },
		Frozen:          func() (bool, string) { return false, "" },
		Admitted:        func() (bool, string) { return true, "" },
		At:              catalogAt, ClusterID: "c1",
	}
}

func with(f catalogFacts, mutate ...func(*catalogFacts)) catalogFacts {
	for _, m := range mutate {
		m(&f)
	}
	return f
}

func changed(name, hash string) func(*catalogFacts) {
	return func(f *catalogFacts) {
		now := map[string]catalogEntry{}
		for k, v := range f.Now {
			now[k] = v
		}
		e := now[name]
		e.Hash = hash
		now[name] = e
		f.Now = now
	}
}

func removed(names ...string) func(*catalogFacts) {
	return func(f *catalogFacts) {
		now := map[string]catalogEntry{}
		for k, v := range f.Now {
			now[k] = v
		}
		for _, n := range names {
			f.RemovedSequence[n] = now[n].Sequence
			delete(now, n)
		}
		f.Now = now
	}
}

func applying(component string, applied bool, startedAgo time.Duration) func(*catalogFacts) {
	return func(f *catalogFacts) {
		p := f.Prev
		f.Prev = &cloudv1.CatalogChange{Applied: p.GetApplied(), Phase: catalogApplying, Component: component, Step: 1,
			ComponentApplied: applied, ComponentStartTime: timestamppb.New(catalogAt.Add(-startedAgo)), StartTime: timestamppb.New(catalogAt.Add(-time.Hour))}
	}
}

func heldNames(d catalogDecision) string {
	var out []string
	for _, n := range []string{"base", "mesh", "policy", "extra", poolEntry} {
		if d.Hold[n] {
			out = append(out, n)
		}
	}
	return strings.Join(out, ",")
}

func TestDecideCatalog(t *testing.T) {
	tests := []struct {
		name string
		f    catalogFacts
		// want: the phase, the entry applied now, the held entries, the
		// pending order, the recorded fingerprints of changed entries.
		phase    cloudv1.CodeChangePhase
		current  string
		held     string
		pending  string
		recorded map[string]string
		recover  string
		message  string
	}{
		{name: "a first pass takes what it renders as applied",
			f:     with(catalogWorld(), func(f *catalogFacts) { f.Prev = nil }, changed("mesh", "m2")),
			phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_UNSPECIFIED, recorded: map[string]string{"mesh": "m2"}},
		{name: "nothing changed", f: catalogWorld(), phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_UNSPECIFIED,
			recorded: map[string]string{"mesh": "m1"}},
		{name: "a values change not admitted waits and holds",
			f: with(catalogWorld(), changed("mesh", "m2"), func(f *catalogFacts) {
				f.Admitted = func() (bool, string) { return false, "waiting for tier 2" }
			}),
			phase: catalogPending, held: "mesh", pending: "mesh", recorded: map[string]string{"mesh": "m1"}, message: "waiting for tier 2"},
		{name: "a frozen cluster waits and holds",
			f: with(catalogWorld(), changed("mesh", "m2"), func(f *catalogFacts) {
				f.Frozen = func() (bool, string) { return true, "the platform is frozen" }
			}),
			phase: catalogPending, held: "mesh", pending: "mesh", message: "the platform is frozen"},
		{name: "an admitted change starts with the lowest sequence; the next waits",
			f:     with(catalogWorld(), changed("mesh", "m2"), changed("base", "b2")),
			phase: catalogApplying, current: "base", held: "mesh", pending: "base,mesh", recorded: map[string]string{"base": "b1", "mesh": "m1"}},
		{name: "the explicit order goes first",
			f: with(catalogWorld(), changed("mesh", "m2"), changed("base", "b2"), changed("policy", "p2"),
				func(f *catalogFacts) { f.Order = []string{"policy", "ghost"} }),
			phase: catalogApplying, current: "policy", held: "base,mesh", pending: "policy,base,mesh"},
		{name: "the entry being applied is not recorded before it settled",
			f:     with(catalogWorld(), changed("base", "b2"), changed("mesh", "m2"), applying("base", true, time.Minute)),
			phase: catalogApplying, current: "base", held: "mesh", recorded: map[string]string{"base": "b1"}},
		{name: "not from the pass that started it, even when it reads settled",
			f: with(catalogWorld(), changed("base", "b2"), changed("mesh", "m2"), applying("base", false, 0),
				func(f *catalogFacts) { f.Settled = func(string) bool { return true } }),
			phase: catalogApplying, current: "base", held: "mesh", recorded: map[string]string{"base": "b1"}},
		{name: "once settled it is recorded and the next one starts",
			f: with(catalogWorld(), changed("base", "b2"), changed("mesh", "m2"), applying("base", true, time.Minute),
				func(f *catalogFacts) { f.Settled = func(n string) bool { return n == "base" } }),
			phase: catalogApplying, current: "mesh", pending: "mesh", recorded: map[string]string{"base": "b2", "mesh": "m1"}},
		{name: "the last one settled: done",
			f: with(catalogWorld(), changed("mesh", "m2"), applying("mesh", true, time.Minute),
				func(f *catalogFacts) { f.Settled = func(n string) bool { return n == "mesh" } }),
			phase: catalogDone, recorded: map[string]string{"mesh": "m2"}},
		{name: "a failed entry is recovered",
			f: with(catalogWorld(), changed("base", "b2"), changed("mesh", "m2"), applying("base", true, time.Minute),
				func(f *catalogFacts) { f.Failed = func(n string) (bool, string) { return n == "base", "chart failed" } }),
			phase: catalogApplying, current: "base", held: "mesh", recover: "base"},
		{name: "a failed entry past its recoveries halts there, before the next",
			f: with(catalogWorld(), changed("base", "b2"), changed("mesh", "m2"), applying("base", true, time.Hour),
				func(f *catalogFacts) {
					f.Prev.Recoveries = maxMoveRecoveries
					f.Failed = func(n string) (bool, string) { return n == "base", "chart failed" }
				}),
			phase: catalogHalted, held: "base,mesh", recorded: map[string]string{"base": "b1", "mesh": "m1"}},
		{name: "an entry that does not settle in time is recovered",
			f:     with(catalogWorld(), changed("base", "b2"), applying("base", true, componentWatch+time.Minute)),
			phase: catalogApplying, current: "base", recover: "base"},
		{name: "a halted change stays halted",
			f: with(catalogWorld(), changed("base", "b2"), func(f *catalogFacts) {
				f.Prev = &cloudv1.CatalogChange{Applied: f.Prev.GetApplied(), Phase: catalogHalted, Component: "base",
					Target: component.Fingerprint(hashes(f.Now)), Message: "base failed"}
			}),
			phase: catalogHalted, held: "base", message: "base failed"},
		{name: "a retry takes a halted change through admission again",
			f: with(catalogWorld(), changed("base", "b2"), func(f *catalogFacts) {
				f.Prev = &cloudv1.CatalogChange{Applied: f.Prev.GetApplied(), Phase: catalogHalted, Component: "base",
					Target: component.Fingerprint(hashes(f.Now))}
				f.Retry = true
			}),
			phase: catalogApplying, current: "base"},
		{name: "a disable removes dependents first, highest sequence first",
			f:     with(catalogWorld(), removed("base", "mesh")),
			phase: catalogApplying, current: "mesh", held: "base", pending: "mesh,base"},
		{name: "removals go before installs and changes",
			f:     with(catalogWorld(), removed("mesh"), changed("base", "b2")),
			phase: catalogApplying, current: "mesh", held: "base", pending: "mesh,base"},
		{name: "a removed entry is done once nothing of it is left",
			f: with(catalogWorld(), removed("mesh"), applying("mesh", true, time.Minute),
				func(f *catalogFacts) { f.Settled = func(n string) bool { return n == "mesh" } }),
			phase: catalogDone, recorded: map[string]string{"mesh": ""}},
		{name: "a removed entry with nothing on the cluster is no change",
			f:     with(catalogWorld(), removed("policy"), func(f *catalogFacts) { f.Settled = func(n string) bool { return n == "policy" } }),
			phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_UNSPECIFIED, recorded: map[string]string{"policy": ""}},
		{name: "an added definition is a change",
			f: with(catalogWorld(), func(f *catalogFacts) {
				f.Now["extra"] = catalogEntry{Hash: component.UnknownFingerprint, Sequence: 400}
				f.Admitted = func() (bool, string) { return false, "" }
			}),
			phase: catalogPending, held: "extra", pending: "extra"},
		{name: "an unknown fingerprint keeps the one applied",
			f:     with(catalogWorld(), changed("mesh", component.UnknownFingerprint)),
			phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_UNSPECIFIED, recorded: map[string]string{"mesh": "m1"}},
		{name: "a change of a component never Ready is taken as it comes",
			f: with(catalogWorld(), changed("mesh", "m2"), func(f *catalogFacts) {
				f.Ready = func(n string) bool { return n != "mesh" }
				f.Admitted = func() (bool, string) { return false, "" }
			}),
			phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_UNSPECIFIED, recorded: map[string]string{"mesh": "m2"}},
		{name: "during a Kubernetes upgrade or a release move it goes with that",
			f: with(catalogWorld(), changed("mesh", "m2"), removed("policy"), func(f *catalogFacts) {
				f.Busy = "a release move is under way"
				f.Admitted = func() (bool, string) { return false, "" }
			}),
			phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_UNSPECIFIED, recorded: map[string]string{"mesh": "m2", "policy": ""}},
		{name: "the platform pool is an entry, before the definitions",
			f: with(catalogWorld(), changed("base", "b2"), func(f *catalogFacts) {
				f.Prev.Applied[poolEntry] = "pool1"
				f.Now[poolEntry] = catalogEntry{Hash: "pool2", Sequence: poolSequence}
			}),
			phase: catalogApplying, current: poolEntry, held: "base", pending: "platform-pool,base"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.f.Prev != nil {
				// Each row gets its own maps.
				applied := map[string]string{}
				for k, v := range tt.f.Prev.GetApplied() {
					applied[k] = v
				}
				tt.f.Prev.Applied = applied
			}
			d := decideCatalog(tt.f)
			st := d.Status
			if st.GetPhase() != tt.phase || d.Current != tt.current || heldNames(d) != tt.held {
				t.Fatalf("phase %v current %q held %q; want %v %q %q (message %q)", st.GetPhase(), d.Current, heldNames(d),
					tt.phase, tt.current, tt.held, st.GetMessage())
			}
			if tt.pending != "" && strings.Join(st.GetPending(), ",") != tt.pending {
				t.Errorf("pending = %v, want %s", st.GetPending(), tt.pending)
			}
			for name, want := range tt.recorded {
				if got := st.GetApplied()[name]; got != want {
					t.Errorf("recorded %s = %q, want %q", name, got, want)
				}
			}
			if d.Recover != tt.recover {
				t.Errorf("recover = %q, want %q", d.Recover, tt.recover)
			}
			if tt.message != "" && st.GetMessage() != tt.message {
				t.Errorf("message = %q, want %q", st.GetMessage(), tt.message)
			}
		})
	}
}

// The order of a change's entries: explicit first, then removals by
// sequence down, then the others by sequence up, ties by name.
func TestCatalogOrder(t *testing.T) {
	now := map[string]catalogEntry{"a": {Sequence: 200}, "b": {Sequence: 100}, "c": {Sequence: 100}, poolEntry: {Sequence: poolSequence}}
	removedSeq := map[string]int32{"x": 50, "y": 300, "dep": 50, "lib": 50}
	requires := map[string][]string{"dep": {"lib"}, "c": {"b2"}}
	now["b2"] = catalogEntry{Sequence: 100}
	tests := []struct {
		name     string
		pending  []string
		explicit []string
		want     string
	}{
		{"by sequence, ties by name", []string{"a", "c", "b"}, nil, "b,c,a"},
		{"removals first, highest sequence first", []string{"a", "x", "y"}, nil, "y,x,a"},
		{"the pool before the definitions", []string{"a", poolEntry}, nil, "platform-pool,a"},
		{"a removed dependent before what it requires, at the same sequence", []string{"lib", "dep"}, nil, "dep,lib"},
		{"what is required before its dependent, at the same sequence", []string{"c", "b2"}, nil, "b2,c"},
		{"an explicit order first; names not pending are skipped", []string{"a", "b", "c"}, []string{"c", "ghost", "a"}, "c,a,b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strings.Join(catalogOrder(tt.pending, now, removedSeq, requires, tt.explicit), ","); got != tt.want {
				t.Errorf("order = %s, want %s", got, tt.want)
			}
		})
	}
}
