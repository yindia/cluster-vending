package cluster

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// catalogPlatform is platformWorld's catalog with a code rollout that
// admits the cluster or not.
type catalogPlatform struct {
	*fakePlatform
	admitted bool
}

func (c *catalogPlatform) ClusterCodeAdmitted(context.Context, string, string, string) (bool, string, error) {
	if c.admitted {
		return true, "", nil
	}
	return false, "waiting for tier 2 to finish", nil
}

func (c *catalogPlatform) NetworkCodeAdmitted(context.Context, string, string) (bool, string, error) {
	return true, "", nil
}

// catalogClusterWorld is platformWorld on the stable channel with base and
// mesh (mesh requires base) installed, deployed and fingerprinted.
func catalogClusterWorld(t *testing.T, opts ...func(*reconcile.Deps)) (world, *catalogPlatform, *model.Cluster) {
	t.Helper()
	w, src := platformWorld(t, opts...)
	cp := &catalogPlatform{fakePlatform: src, admitted: true}
	w.svc.deps.Platform = cp
	w.helmConfig(t)
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.ReleaseChannel = cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE
	if row.Spec, err = Encode(spec); err != nil {
		t.Fatal(err)
	}
	row = w.pass(t, row)
	w.deployed(t, "base")
	row = w.pass(t, row)
	w.deployed(t, "mesh")
	row = w.pass(t, row)
	if got := phases(storedPlatform(row)); got != "base=READY,mesh=READY" {
		t.Fatalf("installed: %s", got)
	}
	if applied := catalogOf(row).GetApplied(); applied["base"] == "" || applied["mesh"] == "" {
		t.Fatalf("not fingerprinted: %v", applied)
	}
	return w, cp, row
}

func catalogOf(row *model.Cluster) *cloudv1.CatalogChange {
	return storedStatus(row).GetCatalogChange()
}

// setValues changes a definition's platformValues.
func (c *catalogPlatform) setValues(t *testing.T, name, values string) {
	t.Helper()
	for i := range c.defs {
		if c.defs[i].Name == name {
			c.defs[i].Spec.PlatformValues = &runtime.RawExtension{Raw: []byte(values)}
			return
		}
	}
	t.Fatalf("no definition %s", name)
}

// replicas is the replicas value of a Release; 0 when it has none.
func (w world) replicas(t *testing.T, name string) int64 {
	t.Helper()
	u := w.release(t, name)
	if u == nil {
		t.Fatalf("no Release %s", name)
	}
	v, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "forProvider", "values", "replicas")
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// A values change of two definitions applies them one after the other,
// what is required first; the first's fingerprint is recorded only once
// its Helm operation settled.
func TestCatalogValuesChangeOneDefinitionAtATime(t *testing.T) {
	w, cp, row := catalogClusterWorld(t)
	before := catalogOf(row).GetApplied()
	cp.setValues(t, "base", `{"replicas": 2}`)
	cp.setValues(t, "mesh", `{"replicas": 3}`)

	row = w.pass(t, row)

	c := catalogOf(row)
	if c.GetPhase() != catalogApplying || c.GetComponent() != "base" || c.GetStep() != 1 || c.GetSteps() != 2 {
		t.Fatalf("catalog change = %v", c)
	}
	if b, m := w.replicas(t, "base"), w.replicas(t, "mesh"); b != 2 || m != 0 {
		t.Fatalf("base %d, mesh %d: want base first, mesh kept", b, m)
	}
	if c.GetApplied()["base"] != before["base"] {
		t.Fatal("base recorded before its Helm operation settled")
	}
	if p := phases(storedPlatform(row)); p != "base=INSTALLING,mesh=READY" && p != "base=READY,mesh=READY" {
		t.Errorf("phases = %s", p)
	}

	w.deployed(t, "base")
	row = w.pass(t, row) // base's state is read from this pass on
	row = w.pass(t, row) // base settled: recorded; mesh starts

	c = catalogOf(row)
	if c.GetComponent() != "mesh" || c.GetStep() != 2 || c.GetApplied()["base"] == before["base"] || c.GetApplied()["mesh"] != before["mesh"] {
		t.Fatalf("after base: %v", c)
	}
	if w.replicas(t, "mesh") != 3 {
		t.Fatalf("mesh not applied after base settled")
	}

	w.deployed(t, "mesh")
	row = w.pass(t, row)
	row = w.pass(t, row)

	if c = catalogOf(row); c.GetPhase() != catalogDone || c.GetApplied()["mesh"] == before["mesh"] || c.GetFinishTime() == nil {
		t.Fatalf("done: %v", c)
	}
}

// A change the code rollout does not admit yet waits, shown as pending,
// and changes nothing on the cluster.
func TestCatalogChangeWaitsForTheRollout(t *testing.T) {
	w, cp, row := catalogClusterWorld(t)
	cp.admitted = false
	cp.setValues(t, "mesh", `{"replicas": 3}`)

	row = w.pass(t, row)

	c := catalogOf(row)
	if c.GetPhase() != catalogPending || c.GetMessage() != "waiting for tier 2 to finish" || w.replicas(t, "mesh") != 0 {
		t.Fatalf("not admitted: %v, mesh %d", c, w.replicas(t, "mesh"))
	}
	if !CodeChanging(row) {
		t.Error("a pending catalog change is part of the code rollout")
	}
	if p := phases(storedPlatform(row)); p != "base=READY,mesh=READY" {
		t.Errorf("phases = %s: what runs stays Ready", p)
	}

	cp.admitted = true
	row = w.pass(t, row)
	if c := catalogOf(row); c.GetPhase() != catalogApplying || w.replicas(t, "mesh") != 3 {
		t.Fatalf("admitted: %v", c)
	}
}

// A failure of the first definition halts the cluster there, after its
// recoveries, before the second is applied.
func TestCatalogFailureHaltsBeforeTheNext(t *testing.T) {
	w, cp, row := catalogClusterWorld(t)
	before := catalogOf(row).GetApplied()["base"]
	at := time.Now()
	now = func() time.Time { return at }
	t.Cleanup(func() { now = time.Now })
	cp.setValues(t, "base", `{"replicas": 2}`)
	cp.setValues(t, "mesh", `{"replicas": 3}`)
	row = w.pass(t, row)

	for i := 0; i < 2*maxMoveRecoveries+4 && catalogOf(row).GetPhase() != catalogHalted; i++ {
		w.failing(t, "base", "failed", "chart failed", at, 1)
		at = at.Add(recoveryGrace + time.Minute)
		row = w.pass(t, row)
	}

	c := catalogOf(row)
	if c.GetPhase() != catalogHalted || c.GetComponent() != "base" || w.replicas(t, "mesh") != 0 {
		t.Fatalf("halted: %v, mesh %d", c, w.replicas(t, "mesh"))
	}
	if c.GetRecoveries() != maxMoveRecoveries || c.GetApplied()["base"] != before {
		t.Errorf("recoveries %d, base recorded %v: want every recovery tried, nothing recorded", c.GetRecoveries(), c.GetApplied()["base"] != before)
	}
}

// A disable switches the dependents off with it: they are uninstalled
// first, one at a time, then the disabled definition.
func TestCatalogDisableRemovesDependentsFirst(t *testing.T) {
	w, cp, row := catalogClusterWorld(t)
	for i := range cp.defs {
		if cp.defs[i].Name == "base" {
			cp.defs[i].Spec.Disabled = true
		}
	}

	row = w.pass(t, row)

	c := catalogOf(row)
	if c.GetPhase() != catalogApplying || c.GetComponent() != "mesh" || c.GetSteps() != 2 {
		t.Fatalf("catalog change = %v", c)
	}
	if w.release(t, "mesh") != nil || w.release(t, "base") == nil {
		t.Fatalf("mesh %v, base %v: want mesh removed first, base kept", w.release(t, "mesh") != nil, w.release(t, "base") != nil)
	}

	row = w.pass(t, row)
	row = w.pass(t, row) // mesh gone: base goes

	if c = catalogOf(row); c.GetComponent() != "base" || w.release(t, "base") != nil {
		t.Fatalf("after mesh: %v, base %v", c, w.release(t, "base") != nil)
	}
	row = w.pass(t, row)
	row = w.pass(t, row)
	if c = catalogOf(row); c.GetPhase() != catalogDone || len(c.GetApplied()) != 0 {
		t.Fatalf("done: %v", c)
	}
}
