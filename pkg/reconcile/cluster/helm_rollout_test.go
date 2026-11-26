package cluster

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/helmrel"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// controllerOf is the Deployment base's chart runs in the workload cluster
// (Helm's ownership annotations), with desired and available replicas.
func controllerOf(generation, observed int64, desired, available int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "base-controller", Namespace: "base-system", Generation: generation,
			Annotations: map[string]string{helmrel.ReleaseNameAnnotation: "base", helmrel.ReleaseNamespaceAnnotation: "base-system"}},
		Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(desired)},
		Status: appsv1.DeploymentStatus{ObservedGeneration: observed, UpdatedReplicas: desired, AvailableReplicas: available},
	}
}

// rolloutWorld is platformWorld whose workload cluster runs base's
// controller as given (mesh's chart has no workloads).
func rolloutWorld(t *testing.T, controller *appsv1.Deployment) (world, *fakePlatform, *fakeWorkload) {
	t.Helper()
	fw := newFakeWorkload(interceptor.Funcs{}, controller)
	w, src := platformWorld(t, func(d *reconcile.Deps) { d.Rollouts = fw })
	return w, src, fw
}

// setController writes the controller's spec generation and status.
func setController(t *testing.T, fw *fakeWorkload, generation, observed int64, available int32) {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := fw.c.Get(t.Context(), client.ObjectKey{Namespace: "base-system", Name: "base-controller"}, d); err != nil {
		t.Fatal(err)
	}
	d.Generation = generation
	if err := fw.c.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	d.Status.ObservedGeneration, d.Status.AvailableReplicas = observed, available
	if err := fw.c.Status().Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	got := &appsv1.Deployment{}
	if err := fw.c.Get(t.Context(), client.ObjectKeyFromObject(d), got); err != nil {
		t.Fatal(err)
	}
	if got.Generation != generation || got.Status.ObservedGeneration != observed || got.Status.AvailableReplicas != available {
		t.Fatalf("controller is generation %d, observed %d, available %d", got.Generation, got.Status.ObservedGeneration, got.Status.AvailableReplicas)
	}
}

// A component is READY only once Helm deployed it and its workloads are
// available; until then it says what it waits for. A chart without
// workloads (mesh) is READY once deployed.
func TestComponentIsReadyOnlyOnceItsWorkloadsAreAvailable(t *testing.T) {
	w, _, fw := rolloutWorld(t, controllerOf(1, 1, 3, 1))
	w.helmConfig(t)
	w.install(t)
	w.deployed(t, "base")

	p := w.install(t)

	base := componentOf(p, "base")
	if base.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING ||
		base.GetMessage() != "waiting for deployment/base-controller: 1/3 available" {
		t.Fatalf("base = %s %q", base.GetPhase(), base.GetMessage())
	}
	mark, ok := helmrel.ParseRolloutMark(w.release(t, "base").GetAnnotations()[helmrel.RolloutAnnotation])
	if !ok || mark.Done || mark.Since.IsZero() {
		t.Fatalf("rollout mark %+v, %v: want the start recorded", mark, ok)
	}

	setController(t, fw, 1, 1, 3)
	p = w.install(t)

	if got := componentOf(p, "base").GetPhase(); got != cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
		t.Fatalf("base = %s, want READY once available: %q", got, componentOf(p, "base").GetMessage())
	}
	if mark, _ := helmrel.ParseRolloutMark(w.release(t, "base").GetAnnotations()[helmrel.RolloutAnnotation]); !mark.Done {
		t.Errorf("rollout mark %+v: want done", mark)
	}
	w.deployed(t, "mesh")
	if p = w.install(t); phases(p) != "base=READY,mesh=READY" {
		t.Errorf("phases = %s, want mesh (no workloads) READY once deployed", phases(p))
	}

	// Rolled out once, it stays READY: later pod churn is not a rollout.
	setController(t, fw, 1, 1, 2)
	if p = w.install(t); phases(p) != "base=READY,mesh=READY" {
		t.Errorf("phases = %s after pod churn", phases(p))
	}
}

// The install order waits for what a component requires to have rolled
// out, not only to be deployed by Helm: mesh (requires base) is not
// created while base's controller is not available.
func TestInstallOrderWaitsForTheRequiredWorkloads(t *testing.T) {
	w, _, fw := rolloutWorld(t, controllerOf(1, 1, 3, 0))
	w.helmConfig(t)
	w.install(t)
	w.deployed(t, "base")

	p := w.install(t)

	if w.release(t, "mesh") != nil {
		t.Fatal("mesh was created before base's workloads were available")
	}
	if mesh := componentOf(p, "mesh"); mesh.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING ||
		!strings.Contains(mesh.GetMessage(), "chart base to roll out (deployment/base-controller: 0/3 available)") {
		t.Errorf("mesh = %s %q", mesh.GetPhase(), mesh.GetMessage())
	}

	setController(t, fw, 1, 1, 3)
	w.install(t)

	if w.release(t, "mesh") == nil {
		t.Fatal("mesh not created once base rolled out")
	}
}

// Past the install timeout after Helm deployed it, a rollout that did not
// finish failed: FAILED with the workload it waited for, a counted release
// error the backoff pauses, as a failed Helm wait was.
func TestARolloutPastItsTimeoutFailsIntoTheBackoff(t *testing.T) {
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return at }
	t.Cleanup(func() { now = time.Now })
	w, _, _ := rolloutWorld(t, controllerOf(1, 1, 3, 1))
	w.helmConfig(t)
	row := w.pass(t, w.cluster(t))
	w.deployed(t, "base")
	row = w.pass(t, row)
	if got := componentOf(storedPlatform(row), "base").GetPhase(); got != cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING {
		t.Fatalf("base = %s within the timeout", got)
	}

	at = at.Add(10*time.Minute + time.Second)
	row = w.pass(t, row)

	base := componentOf(storedPlatform(row), "base")
	if base.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED ||
		!strings.Contains(base.GetMessage(), "rollout timed out after 10m0s waiting for deployment/base-controller") {
		t.Fatalf("base = %s %q", base.GetPhase(), base.GetMessage())
	}
	if base.GetLastErrorClass() != string(helmrel.ClassRelease) || base.GetAttempts() != 1 || base.GetCountedFailures() != 1 ||
		base.GetNextRetryAt() == nil {
		t.Errorf("backoff: class %q attempts %d counted %d next %v", base.GetLastErrorClass(), base.GetAttempts(),
			base.GetCountedFailures(), base.GetNextRetryAt())
	}
	if w.release(t, "base").GetAnnotations()[helmrel.BackoffAnnotation] == "" {
		t.Error("the timed-out Release is not paused by the backoff")
	}
}

// A catalog change's entry is recorded (its fingerprint) only once its
// component's workloads rolled out, not when Helm deployed the chart.
func TestCatalogFingerprintWaitsForTheWorkloads(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{}, controllerOf(1, 1, 3, 3))
	w, cp, row := catalogClusterWorld(t, func(d *reconcile.Deps) { d.Rollouts = fw })
	before := catalogOf(row).GetApplied()["base"]
	cp.setValues(t, "base", `{"replicas": 2}`)
	row = w.pass(t, row)
	if w.replicas(t, "base") != 2 {
		t.Fatal("base's change not applied")
	}

	setController(t, fw, 2, 1, 3) // the new spec is not rolled out yet
	w.deployed(t, "base")
	row = w.pass(t, row)
	row = w.pass(t, row)

	if catalogOf(row).GetApplied()["base"] != before {
		t.Fatal("base recorded although its workloads did not roll out")
	}
	if base := componentOf(storedPlatform(row), "base"); base.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING ||
		!strings.Contains(base.GetMessage(), "rollout not observed yet") {
		t.Errorf("base = %s %q", base.GetPhase(), base.GetMessage())
	}

	setController(t, fw, 2, 2, 3)
	row = w.pass(t, row)
	row = w.pass(t, row)

	if catalogOf(row).GetApplied()["base"] == before {
		t.Error("base not recorded once its workloads rolled out")
	}
}

// Without workload clients (tests, or a build that cannot reach the
// clusters) a Release is Ready once Helm deployed it.
func TestRolloutWithoutWorkloadClientsIsHelmsWord(t *testing.T) {
	g := &helmGate{unchanged: map[string]bool{}}
	d := settledAt("abc")
	if r := g.rollout(t.Context(), "x", d); !r.Ready {
		t.Errorf("deployed: %+v", r)
	}
	d.Synced, d.LastError = false, "upgrade failed: boom"
	if r := g.rollout(t.Context(), "x", d); r.Ready || !r.Failed || r.Message != "upgrade failed: boom" {
		t.Errorf("failed upgrade: %+v, want it failed with Helm's error", r)
	}
	d = settledAt("abc")
	d.behind = true
	if r := g.rollout(t.Context(), "x", d); r.Ready || r.Failed {
		t.Errorf("behind: %+v, want waiting", r)
	}
}
