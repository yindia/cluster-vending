package cluster

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// settled installs and deploys base, then mesh, as provider-helm would.
func (w world) settled(t *testing.T) {
	t.Helper()
	w.helmConfig(t)
	w.install(t)
	w.deployed(t, "base")
	w.install(t)
	w.deployed(t, "mesh")
	w.install(t)
}

func (w world) version(t *testing.T, name string) string {
	t.Helper()
	return w.release(t, name).GetResourceVersion()
}

// A pass over a cluster whose charts keep their version and values writes
// no Release at all, and says so.
func TestIdenticalComponentsWriteNoRelease(t *testing.T) {
	w, _ := platformWorld(t)
	w.settled(t)
	base, mesh := w.version(t, "base"), w.version(t, "mesh")

	p := w.install(t)

	if w.version(t, "base") != base || w.version(t, "mesh") != mesh {
		t.Error("a Release was written although nothing changed")
	}
	for _, c := range p.components {
		if !c.GetUnchanged() || c.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			t.Errorf("%s: unchanged=%v phase=%s", c.GetName(), c.GetUnchanged(), c.GetPhase())
		}
	}
}

// With one component's version moved, only its Release is written.
func TestOnlyTheChangedReleaseIsWritten(t *testing.T) {
	w, src := platformWorld(t)
	w.settled(t)
	base, mesh := w.version(t, "base"), w.version(t, "mesh")
	src.release.Spec.Content.Kubernetes[0].Components[0].Version = "1.3.0"

	p := w.install(t)

	if w.version(t, "base") == base {
		t.Error("the changed Release was not written")
	}
	if got, _, _ := nestedChartVersion(w.release(t, "base")); got != "1.3.0" {
		t.Errorf("chart version %q", got)
	}
	if w.version(t, "mesh") != mesh {
		t.Error("an unchanged Release was written")
	}
	if componentOf(p, "base").GetUnchanged() || !componentOf(p, "mesh").GetUnchanged() {
		t.Errorf("unchanged: base %v mesh %v", componentOf(p, "base").GetUnchanged(), componentOf(p, "mesh").GetUnchanged())
	}
}

// Values count too: the digest covers the rendered values.
func TestChangedValuesWriteTheRelease(t *testing.T) {
	w, src := platformWorld(t)
	w.settled(t)
	base := w.version(t, "base")
	src.defs[1].Spec.PlatformValues = rawValues(`{"replicas":3}`)

	w.install(t)

	if w.version(t, "base") == base {
		t.Error("new values did not reach the Release")
	}
}

// Two changed Releases are written one at a time, in install order: the
// second waits for the first to be deployed and Ready, and reads as not
// settled meanwhile.
func TestChangesAreWrittenOneAtATime(t *testing.T) {
	atStuckTime(t) // base's operation below started just now: still running
	w, src := platformWorld(t)
	w.settled(t)
	mesh := w.version(t, "mesh")
	src.release.Spec.Content.Kubernetes[0].Components[0].Version = "1.3.0"
	src.release.Spec.Content.Kubernetes[0].Components[1].Version = "2.1.0"

	p := w.install(t)

	if w.version(t, "mesh") != mesh {
		t.Fatal("mesh was upgraded together with base")
	}
	mc := componentOf(p, "mesh")
	if mc.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_READY || !strings.Contains(mc.GetMessage(), "one Helm operation per cluster at a time") ||
		!strings.Contains(mc.GetMessage(), "base") {
		t.Errorf("mesh = %s %q, want it waiting for base", mc.GetPhase(), mc.GetMessage())
	}
	// provider-helm has started upgrading base: it is not Ready.
	w.failing(t, "base", "pending-upgrade", "", stuckAt, 0)
	w.install(t)
	if w.version(t, "mesh") != mesh {
		t.Fatal("mesh was upgraded while base is still not deployed")
	}
	w.deployed(t, "base")
	w.install(t)
	if w.version(t, "mesh") == mesh {
		t.Error("mesh was not upgraded once base settled")
	}
}

// Brand new Releases are serialised too: of two components that wait for
// nothing but each other's sequence, one is created, the other once it is
// deployed.
func TestNewReleasesStartOneAtATime(t *testing.T) {
	w, src := platformWorld(t)
	src.defs = append(src.defs, platformDef("extra"))
	src.release.Spec.Content.Kubernetes[0].Components = append(src.release.Spec.Content.Kubernetes[0].Components,
		pin("extra", "0.1.0"))
	w.helmConfig(t)

	p := w.install(t)

	created := 0
	for _, name := range []string{"base", "extra"} {
		if w.release(t, name) != nil {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d of base and extra were created in one pass, want 1", created)
	}
	waiting := componentOf(p, "extra")
	if w.release(t, "extra") != nil {
		waiting = componentOf(p, "base")
	}
	if !strings.Contains(waiting.GetMessage(), "one Helm operation per cluster at a time") {
		t.Errorf("message %q", waiting.GetMessage())
	}
}

// A Release given up on holds nothing back; one in backoff does.
func TestGivenUpReleaseDoesNotBlockTheRest(t *testing.T) {
	w, src := platformWorld(t)
	w.settled(t)
	mesh := w.version(t, "mesh")
	src.release.Spec.Content.Kubernetes[0].Components[1].Version = "2.1.0"
	w.failing(t, "base", "failed", gpuErr, stuckAt, 3)
	if w.install(t); w.version(t, "mesh") != mesh {
		t.Fatal("mesh was written while base fails and is retried")
	}
	u := w.release(t, "base")
	annotations := u.GetAnnotations()
	annotations["cloud.cluster-vending.io/helm-gave-up"] = "true"
	u.SetAnnotations(annotations)
	if err := w.kube.Update(t.Context(), u); err != nil {
		t.Fatal(err)
	}

	w.install(t)

	if w.version(t, "mesh") == mesh {
		t.Error("a Release the platform gave up on still blocks the cluster")
	}
}

func nestedChartVersion(u interface{ UnstructuredContent() map[string]any }) (string, bool, error) {
	spec, _ := u.UnstructuredContent()["spec"].(map[string]any)
	fp, _ := spec["forProvider"].(map[string]any)
	chart, _ := fp["chart"].(map[string]any)
	v, ok := chart["version"].(string)
	return v, ok, nil
}

func pin(name, version string) cloudv1alpha1.ComponentPin {
	return cloudv1alpha1.ComponentPin{Name: name, Version: version}
}

func rawValues(js string) *runtime.RawExtension { return &runtime.RawExtension{Raw: []byte(js)} }
