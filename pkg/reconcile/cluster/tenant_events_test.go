package cluster

import (
	"testing"

	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func TestTenantLifecycleEventsFollowCommittedTransitions(t *testing.T) {
	w, _, component := tenantWorld(t)
	row := w.cluster(t)
	for _, step := range []struct {
		phase           v1.ComponentPhase
		version, reason string
	}{
		{v1.ComponentPhase_COMPONENT_PHASE_INSTALLING, "1", "ComponentInstalling"},
		{v1.ComponentPhase_COMPONENT_PHASE_READY, "1", "ComponentReady"},
		{v1.ComponentPhase_COMPONENT_PHASE_INSTALLING, "2", "ComponentUpgrading"},
		{v1.ComponentPhase_COMPONENT_PHASE_READY, "2", "ComponentUpgraded"},
		{v1.ComponentPhase_COMPONENT_PHASE_DELETING, "2", "ComponentDeleting"},
	} {
		current, err := w.store.Components().Get(t.Context(), worldTenant, component.ID)
		if err != nil {
			t.Fatal(err)
		}
		ctx := reconcile.WithNotes(t.Context())
		status := &v1.PlatformComponent{Name: current.Component, Version: step.version, Phase: step.phase}
		if err := w.svc.setTenantStatus(ctx, row, *current, status, "release"); err != nil {
			t.Fatal(err)
		}
		notes := reconcile.Notes(ctx)
		if len(notes) != 1 || notes[0].Reason != step.reason {
			t.Fatalf("want %s after status commit: %+v", step.reason, notes)
		}
		current, err = w.store.Components().Get(t.Context(), worldTenant, component.ID)
		if err != nil {
			t.Fatal(err)
		}
		repeated := reconcile.WithNotes(t.Context())
		if err := w.svc.setTenantStatus(repeated, row, *current, status, "release"); err != nil {
			t.Fatal(err)
		}
		if notes := reconcile.Notes(repeated); len(notes) != 0 {
			t.Fatalf("unchanged status emitted events: %+v", notes)
		}
	}
	current, err := w.store.Components().Get(t.Context(), worldTenant, component.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.ETag = "stale"
	conflict := reconcile.WithNotes(t.Context())
	if err := w.svc.setTenantStatus(conflict, row, *current, &v1.PlatformComponent{Name: current.Component, Version: "3", Phase: v1.ComponentPhase_COMPONENT_PHASE_INSTALLING}, "release"); err != nil {
		t.Fatal(err)
	}
	if notes := reconcile.Notes(conflict); len(notes) != 0 {
		t.Fatalf("uncommitted transition emitted events: %+v", notes)
	}
}
