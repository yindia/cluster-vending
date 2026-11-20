package cluster

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/helmrel"
)

// firstInstallOrder makes base the first component (sequence 0, as
// Karpenter is); mesh stays at the default sequence and requires it.
func firstInstallOrder(src *fakePlatform) {
	zero := int32(0)
	src.defs[1].Spec.Sequence = &zero
}

// The live cluster: a component of a higher sequence (capsule) is in the
// middle of an operation, and the first component (Karpenter) must change.
// It never waits behind the one that comes after it.
func TestAReleaseNeverWaitsBehindOneOfAHigherSequence(t *testing.T) {
	atStuckTime(t)
	w, src := platformWorld(t)
	firstInstallOrder(src)
	w.settled(t)
	base := w.version(t, "base")
	src.release.Spec.Content.Kubernetes[0].Components[0].Version = "1.3.0"
	w.failing(t, "mesh", "pending-upgrade", "", now(), 0) // running, within its timeout

	w.install(t)

	if w.version(t, "base") == base {
		t.Error("the first component waits behind a component installed after it")
	}
}

// What is not an operation in flight blocks nobody: a stuck one past its
// timeout, one given up, one paused by anyone, one in its backoff.
func TestOnlyARunningOperationBlocks(t *testing.T) {
	for _, tt := range []struct {
		name  string
		state func(t *testing.T, w world)
		block bool
	}{
		{"running within its timeout", func(t *testing.T, w world) { w.failing(t, "base", "pending-upgrade", "", now(), 0) }, true},
		{"stuck past its timeout and recovery", func(t *testing.T, w world) {
			w.failing(t, "base", "pending-upgrade", "", stuckAt.Add(-time.Hour), 0)
		}, false},
		{"failed, in backoff", func(t *testing.T, w world) {
			w.failing(t, "base", "failed", gpuErr, stuckAt.Add(-time.Second), 1)
			w.annotate(t, "base", managed.PausedAnnotation, "true", helmrel.BackoffAnnotation, stuckAt.Add(time.Minute).Format(time.RFC3339))
		}, false},
		{"paused by an operator", func(t *testing.T, w world) {
			w.failing(t, "base", "pending-upgrade", "", now(), 0)
			w.annotate(t, "base", managed.PausedAnnotation, "true")
		}, false},
		{"given up", func(t *testing.T, w world) {
			w.failing(t, "base", "failed", gpuErr, stuckAt.Add(-time.Second), 5)
			w.annotate(t, "base", helmrel.GaveUpAnnotation, "true")
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			atStuckTime(t)
			w, src := platformWorld(t)
			w.settled(t)
			mesh := w.version(t, "mesh")
			src.release.Spec.Content.Kubernetes[0].Components[1].Version = "2.1.0"
			tt.state(t, w)

			p := w.install(t)

			if written := w.version(t, "mesh") != mesh; written == tt.block {
				t.Errorf("mesh written = %v, want %v (%q)", written, !tt.block, componentOf(p, "mesh").GetMessage())
			}
		})
	}
}

// annotate sets annotations (key, value, ...) on a Release.
func (w world) annotate(t *testing.T, name string, kv ...string) {
	t.Helper()
	u := w.release(t, name)
	annotations := u.GetAnnotations()
	for i := 0; i+1 < len(kv); i += 2 {
		annotations[kv[i]] = kv[i+1]
	}
	u.SetAnnotations(annotations)
	if err := w.kube.Update(t.Context(), u); err != nil {
		t.Fatal(err)
	}
}

func chartPlan(name string) crossplaneaws.HelmReleasePlan {
	return crossplaneaws.HelmReleasePlan{Name: name, Chart: crossplaneaws.HelmChart{Repository: "https://charts.example.com", Name: name, Version: "1.0.0"}}
}

// gateOver is a gate over live Releases (name -> detail) and a plan.
func gateOver(plan crossplaneaws.PlatformPlan, live map[string]*releaseDetail, tenant bool) *helmGate {
	g := &helmGate{live: live, unchanged: map[string]bool{}}
	g.once.Do(func() {})
	for n := range live {
		g.names = append(g.names, n)
	}
	g.register(plan, tenant)
	return g
}

func settledAt(digest string) *releaseDetail {
	return &releaseDetail{Digest: digest, Ready: true, Synced: true, HelmState: crossplaneaws.HelmDeployed}
}

func releaseWanting(plan crossplaneaws.PlatformPlan, component string) *unstructured.Unstructured {
	for _, c := range plan.Components {
		if c.Name == component {
			for name, digest := range plan.ReleaseDigests(c) {
				u := &unstructured.Unstructured{}
				u.SetName(name)
				u.SetAnnotations(map[string]string{helmrel.DigestAnnotation: digest})
				return u
			}
		}
	}
	return nil
}

// When several Releases must change the lowest sequence goes first, one
// that is required first as well; a higher one never holds a lower one.
func TestTheLowestSequenceChangesFirst(t *testing.T) {
	plan := crossplaneaws.PlatformPlan{ClusterPrefix: "p", Components: []crossplaneaws.PlatformComponentPlan{
		{Name: "karpenter", Sequence: 0, Main: chartPlan("karpenter")},
		{Name: "alpha", Sequence: 100, Main: chartPlan("alpha")},
		{Name: "beta", Sequence: 100, Requires: []string{"alpha"}, Main: chartPlan("beta")},
		{Name: "gamma", Sequence: 100, Requires: []string{"beta"}, Main: chartPlan("gamma")},
	}}
	obj := func(c string) string { return plan.ReleaseObjectNames(plan.Components[indexOf(plan, c)])[0] }
	live := func(changed ...string) map[string]*releaseDetail {
		out := map[string]*releaseDetail{}
		for _, c := range plan.Components {
			out[obj(c.Name)] = settledAt(plan.ReleaseDigests(c)[obj(c.Name)])
		}
		for _, c := range changed {
			out[obj(c)] = settledAt("older")
		}
		return out
	}
	for _, tt := range []struct {
		name    string
		changed []string
		decide  string
		holdFor string // "" goes now
	}{
		{"karpenter and alpha change: alpha waits", []string{"karpenter", "alpha"}, "alpha", "karpenter"},
		{"karpenter and alpha change: karpenter goes", []string{"karpenter", "alpha"}, "karpenter", ""},
		{"only alpha changes: it goes", []string{"alpha"}, "alpha", ""},
		{"alpha, beta and gamma change: gamma waits for the first", []string{"alpha", "beta", "gamma"}, "gamma", "alpha"},
		{"beta and gamma change, alpha is current: beta goes", []string{"beta", "gamma"}, "beta", ""},
		{"beta requires alpha: alpha never waits for beta", []string{"alpha", "beta"}, "alpha", ""},
		{"beta requires alpha: beta waits for alpha", []string{"alpha", "beta"}, "beta", "alpha"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := gateOver(plan, live(tt.changed...), false)

			d := g.decide(t.Context(), releaseWanting(plan, tt.decide))

			switch {
			case tt.holdFor == "" && d.Hold != "":
				t.Errorf("held: %s", d.Hold)
			case tt.holdFor != "" && !strings.Contains(d.Hold, tt.holdFor):
				t.Errorf("hold = %q, want it to wait for %s", d.Hold, tt.holdFor)
			}
		})
	}
}

func indexOf(plan crossplaneaws.PlatformPlan, name string) int {
	for i, c := range plan.Components {
		if c.Name == name {
			return i
		}
	}
	return -1
}
