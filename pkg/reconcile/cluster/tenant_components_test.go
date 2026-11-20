package cluster

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// tenantWorld is platformWorld with the tenant component gpu requiring
// the platform's base, pinned by the release, and enabled on the cluster
// with values of the tenant's.
func tenantWorld(t *testing.T) (world, *fakePlatform, *model.Component) {
	t.Helper()
	w, src := platformWorld(t)
	for i := range src.defs {
		if src.defs[i].Name == "gpu" {
			src.defs[i].Spec.Requires = []string{"base"}
		}
	}
	k := &src.release.Spec.Content.Kubernetes[0]
	k.Components = append(k.Components, cloudv1alpha1.ComponentPin{Name: "gpu", Version: "26.7.2"})
	c := &model.Component{TenantID: worldTenant, ClusterID: w.row.ID, Component: "gpu", State: "COMPONENT_PHASE_PENDING",
		Values: model.JSON(`{"mig":{"strategy":"single"}}`)}
	if err := w.store.Components().Create(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	return w, src, c
}

// tenantPass is one pass over the cluster: platform, then tenant
// components, as Reconcile runs them.
func (w world) tenantPass(t *testing.T) {
	t.Helper()
	row := w.cluster(t)
	p, err := w.svc.installPlatform(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.installTenant(t.Context(), w.cluster(t), p); err != nil {
		t.Fatal(err)
	}
}

func (w world) componentStatus(t *testing.T, id string) (*model.Component, *cloudv1.ComponentStatus) {
	t.Helper()
	c, err := w.store.Components().Get(t.Context(), worldTenant, id)
	if err != nil {
		t.Fatal(err)
	}
	st := &cloudv1.ComponentStatus{}
	if len(c.Status) > 0 {
		if err := protojson.Unmarshal(c.Status, st); err != nil {
			t.Fatal(err)
		}
	}
	return c, st
}

// baseDeployed installs the platform's components and marks each deployed
// (base, then mesh, which installs once base is): tenant components start
// only once every platform component is Ready.
func (w world) baseDeployed(t *testing.T) {
	t.Helper()
	w.helmConfig(t)
	w.tenantPass(t)
	w.deployed(t, "base")
	w.tenantPass(t)
	w.deployed(t, "mesh")
}

// A tenant component waits for the platform component it requires, then
// installs at the release's version with the tenant's values, recorded
// with the Component, and reports each phase.
func TestTenantComponentInstalls(t *testing.T) {
	w, _, c := tenantWorld(t)
	w.helmConfig(t)

	w.tenantPass(t)
	if _, st := w.componentStatus(t, c.ID); st.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING ||
		!strings.Contains(st.GetMessage(), "waiting for") || w.release(t, "gpu") != nil {
		t.Fatalf("before base is deployed: %v %q", st.GetPhase(), st.GetMessage())
	}

	w.deployed(t, "base")
	w.tenantPass(t) // mesh installs once base is deployed
	w.deployed(t, "mesh")
	w.tenantPass(t)
	rel := w.release(t, "gpu")
	if rel == nil {
		t.Fatal("gpu is not installed once base is deployed")
	}
	version, _, _ := unstructured.NestedString(rel.Object, "spec", "forProvider", "chart", "version")
	strategy, _, _ := unstructured.NestedString(rel.Object, "spec", "forProvider", "values", "mig", "strategy")
	if version != "26.7.2" || strategy != "single" {
		t.Errorf("chart %s with mig.strategy %q, want 26.7.2 and the tenant's single", version, strategy)
	}
	if _, st := w.componentStatus(t, c.ID); st.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING {
		t.Fatalf("installing: %v %q", st.GetPhase(), st.GetMessage())
	}
	records, err := w.store.ManagedResources().ListByOwner(t.Context(), model.OwnerComponent, c.ID)
	if err != nil || len(records) == 0 {
		t.Fatalf("the component's objects are not recorded with it: %v, %v", records, err)
	}

	w.deployed(t, "gpu")
	w.tenantPass(t)
	stored, st := w.componentStatus(t, c.ID)
	if st.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY || st.GetInstalledVersion() != "26.7.2" ||
		st.GetObservedGeneration() != stored.Generation || st.GetRelease() != "2026-11-a" || st.GetSince() == nil {
		t.Fatalf("ready: %+v", st)
	}
}

func TestTenantComponentNotInstalled(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(*testing.T, world, *fakePlatform)
		phase cloudv1.ComponentPhase
		want  string
	}{
		{"not in the release", func(_ *testing.T, _ world, f *fakePlatform) {
			k := &f.release.Spec.Content.Kubernetes[0]
			k.Components = k.Components[:2]
		}, cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, "not in release 2026-11-a"},
		{"not in the catalog", func(_ *testing.T, _ world, f *fakePlatform) {
			f.defs = f.defs[:2]
		}, cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, "not in the catalog"},
		{"imported read-only", func(t *testing.T, w world, _ *fakePlatform) {
			row := w.cluster(t)
			row.Imported, row.State = true, StateImported
			if err := w.store.Clusters().Update(t.Context(), row); err != nil {
				t.Fatal(err)
			}
		}, cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING, "imported read-only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, src, c := tenantWorld(t)
			w.helmConfig(t)
			tt.edit(t, w, src)

			w.tenantPass(t)

			_, st := w.componentStatus(t, c.ID)
			if st.GetPhase() != tt.phase || !strings.Contains(st.GetMessage(), tt.want) || w.release(t, "gpu") != nil {
				t.Fatalf("%v %q, want %v with %q and nothing installed", st.GetPhase(), st.GetMessage(), tt.phase, tt.want)
			}
		})
	}
}

// A second pass that changes nothing keeps the phase's time.
func TestTenantComponentStatusIsStable(t *testing.T) {
	w, _, c := tenantWorld(t)
	w.baseDeployed(t)
	w.tenantPass(t)
	_, first := w.componentStatus(t, c.ID)

	w.tenantPass(t)

	_, second := w.componentStatus(t, c.ID)
	if !first.GetSince().AsTime().Equal(second.GetSince().AsTime()) || first.GetPhase() != second.GetPhase() {
		t.Errorf("since %v then %v", first.GetSince(), second.GetSince())
	}
}

// A disabled component's charts are removed, then the Component itself.
func TestTenantComponentDisabled(t *testing.T) {
	w, _, c := tenantWorld(t)
	w.baseDeployed(t)
	w.tenantPass(t)
	if w.release(t, "gpu") == nil {
		t.Fatal("gpu is not installed")
	}
	stored, _ := w.componentStatus(t, c.ID)
	stored.State = componentDeleting
	if err := w.store.Components().Update(t.Context(), stored); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		w.tenantPass(t)
	}

	if w.release(t, "gpu") != nil {
		t.Error("gpu's chart is still there")
	}
	if w.release(t, "base") == nil {
		t.Error("the platform's base went with it")
	}
	if _, err := w.store.Components().Get(t.Context(), worldTenant, c.ID); err == nil {
		t.Error("the component is still stored")
	}
}

// A cluster being deleted removes its tenant components first: every one
// is marked for removal and removed, and the deletion goes on only once
// they are gone.
func TestClusterDeleteRemovesTenantComponentsFirst(t *testing.T) {
	w, _, c := tenantWorld(t)
	w.baseDeployed(t)
	w.tenantPass(t)
	row := w.cluster(t)

	gone := false
	for range 3 {
		var err error
		if gone, err = w.svc.deleteTenant(t.Context(), row); err != nil {
			t.Fatal(err)
		}
		if gone {
			break
		}
	}

	if !gone || w.release(t, "gpu") != nil || w.release(t, "base") == nil {
		t.Fatalf("gone %v; gpu removed, base kept until the platform's turn", gone)
	}
	if _, err := w.store.Components().Get(t.Context(), worldTenant, c.ID); err == nil {
		t.Error("the component is still stored")
	}
}

// EKS add-ons, then platform components, then tenant components: a tenant
// component is not installed while any platform component is not Ready,
// even one it does not require, and says what it waits for.
func TestTenantComponentWaitsForEveryPlatformComponent(t *testing.T) {
	w, _, c := tenantWorld(t)
	w.helmConfig(t)
	w.tenantPass(t)
	w.deployed(t, "base") // gpu's own requirement, but mesh is not Ready yet

	w.tenantPass(t)
	w.tenantPass(t) // mesh is installed now, not Ready

	if _, st := w.componentStatus(t, c.ID); st.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING ||
		!strings.Contains(st.GetMessage(), "waiting for the platform components to be Ready: mesh") || w.release(t, "gpu") != nil {
		t.Fatalf("with mesh not Ready: %v %q, want PENDING and nothing installed", st.GetPhase(), st.GetMessage())
	}

	w.deployed(t, "mesh")
	w.tenantPass(t)

	if w.release(t, "gpu") == nil {
		t.Fatal("gpu is not installed once every platform component is Ready")
	}
}
