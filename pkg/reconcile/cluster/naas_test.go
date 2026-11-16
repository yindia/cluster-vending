package cluster

import (
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func (w world) setNaaS(t *testing.T, enabled bool) {
	t.Helper()
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.Naas = ptr.To(enabled)
	row.Spec, err = Encode(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
}

func TestNaaSToggleInstallsAndRemovesOnlyCapsule(t *testing.T) {
	w, source := platformWorld(t)
	source.defs = []cloudv1alpha1.ComponentDefinition{platformDef("base"), platformDef("capsule")}
	source.release.Spec.Content.Kubernetes[0].Components = []cloudv1alpha1.ComponentPin{
		{Name: "base", Version: "1.2.0"}, {Name: "capsule", Version: "0.15.2"},
	}
	w.helmConfig(t)
	// Omitted/false never installs Capsule or reports it as a failed component.
	if got := phases(w.install(t)); strings.Contains(got, "capsule") || w.release(t, "capsule") != nil {
		t.Fatalf("NaaS defaults off: %s", got)
	}
	w.deployed(t, "base")
	w.setNaaS(t, true)
	if got := phases(w.install(t)); !strings.Contains(got, "capsule=INSTALLING") || w.release(t, "capsule") == nil {
		t.Fatalf("enabling NaaS did not install Capsule: %s", got)
	}
	w.deployed(t, "capsule")
	w.setNaaS(t, false)
	if got := phases(w.install(t)); strings.Contains(got, "capsule=FAILED") {
		t.Fatalf("disabled Capsule reported as failed: %s", got)
	}
	if capsule := w.release(t, "capsule"); capsule != nil && capsule.GetDeletionTimestamp() == nil {
		t.Fatal("disabling NaaS did not request the managed Capsule release's deletion")
	}
	if base := w.release(t, "base"); base == nil || base.GetDeletionTimestamp() != nil {
		t.Fatal("NaaS toggle removed an unrelated platform component")
	}
}

func TestNaaSDisableWaitsForUnfreeze(t *testing.T) {
	w, source := platformWorld(t)
	source.defs = []cloudv1alpha1.ComponentDefinition{platformDef("capsule")}
	source.release.Spec.Content.Kubernetes[0].Components = []cloudv1alpha1.ComponentPin{{Name: "capsule", Version: "0.15.2"}}
	w.helmConfig(t)
	w.setNaaS(t, true)
	w.install(t)
	w.deployed(t, "capsule")
	w.setNaaS(t, false)
	w.pause(t, alice)
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if capsule := w.release(t, "capsule"); capsule == nil || capsule.GetDeletionTimestamp() != nil {
		t.Fatal("Capsule was removed while the cluster was frozen")
	}
	w.pause(t, nil)
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if capsule := w.release(t, "capsule"); capsule != nil && capsule.GetDeletionTimestamp() == nil {
		t.Fatal("Capsule removal did not resume after unfreeze")
	}
}

func TestNaaSDisabledDoesNotBlockReleaseMove(t *testing.T) {
	w, source := platformWorld(t)
	source.defs = append(source.defs, platformDef("capsule"))
	source.release.Spec.Content.Kubernetes[0].Components = append(source.release.Spec.Content.Kubernetes[0].Components,
		cloudv1alpha1.ComponentPin{Name: "capsule", Version: "0.15.2"})
	in, _, problem, err := w.svc.platformInput(t.Context(), w.cluster(t), false, false)
	if err != nil || problem != "" {
		t.Fatalf("platform input: %v, %s", err, problem)
	}
	for _, entry := range in.Entries {
		if entry.Name == "capsule" {
			t.Fatal("disabled Capsule entered the release move order")
		}
	}
	// Its pin may remain in the shared release; selection is per cluster.
	if in.Pins["capsule"].Version != "0.15.2" {
		t.Fatal("cluster feature filtering changed the shared release")
	}
}

func TestNaaSTogglesWaitForActiveReleaseMovesWithoutChangingTheirComponentOrder(t *testing.T) {
	entries := []component.PlatformEntry{
		{Definition: component.Definition{Name: "base"}},
		{Definition: component.Definition{Name: "capsule"}},
	}
	for _, phase := range []cloudv1.ClusterReleaseMovePhase{moveComponents, moveHalted} {
		for _, wasEnabled := range []bool{false, true} {
			status := &cloudv1.ClusterStatus{ReleaseMove: &cloudv1.ClusterReleaseMove{Phase: phase, Step: 2, Steps: 3, Component: "base"}}
			if wasEnabled {
				status.PlatformComponents = []*cloudv1.PlatformComponent{{Name: "capsule", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY}}
			}
			raw, err := Encode(status)
			if err != nil {
				t.Fatal(err)
			}
			row := &model.Cluster{Resource: model.Resource{Status: raw}}
			if got := naasForPass(!wasEnabled, row, entries); got != wasEnabled {
				t.Fatalf("changed component order during %s: was enabled %v, got %v", phase, wasEnabled, got)
			}
			status.ReleaseMove.Phase = moveIdle
			row.Status, err = Encode(status)
			if err != nil {
				t.Fatal(err)
			}
			if got := naasForPass(!wasEnabled, row, entries); got == wasEnabled {
				t.Fatal("queued feature change was not applied when the move finished")
			}
		}
	}
}
