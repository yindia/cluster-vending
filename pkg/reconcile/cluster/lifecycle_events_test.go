package cluster

import (
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestManagedResourceLifecycleEvents(t *testing.T) {
	for _, kind := range []string{"NodeGroup", "Addon"} {
		t.Run(kind, func(t *testing.T) {
			snapshot := func(state v1.ManagedResourceState, generation, observed int64) *v1.ClusterStatus {
				return &v1.ClusterStatus{ManagedResources: []*v1.ManagedResource{{Kind: kind, Name: "resource", State: state, Generation: generation, ObservedGeneration: observed}}}
			}
			creating := snapshot(v1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING, 1, 0)
			ready := snapshot(v1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, 1, 1)
			updating := snapshot(v1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING, 2, 1)
			upgraded := snapshot(v1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, 2, 2)
			deleting := snapshot(v1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING, 2, 2)
			missing := snapshot(v1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING, 2, 2)
			start, complete := "Creating", "Created"
			if kind == "Addon" {
				start, complete = "Installing", "Installed"
			}
			for _, test := range []struct {
				before, after *v1.ClusterStatus
				reason        string
			}{
				{&v1.ClusterStatus{}, creating, kind + start}, {creating, ready, kind + complete},
				{ready, updating, kind + "Upgrading"}, {updating, upgraded, kind + "Upgraded"},
				{upgraded, deleting, kind + "Deleting"}, {deleting, missing, kind + "Deleted"},
			} {
				notes := managedLifecycleEvents(test.before, test.after)
				if len(notes) != 1 || notes[0].Reason != test.reason {
					t.Fatalf("want %s: %+v", test.reason, notes)
				}
				if repeated := managedLifecycleEvents(test.after, proto.CloneOf(test.after)); len(repeated) != 0 {
					t.Fatalf("replayed lifecycle event: %+v", repeated)
				}
			}
			updating.ManagedResources[0].Paused = true
			if notes := managedLifecycleEvents(ready, updating); len(notes) != 0 {
				t.Fatalf("paused update emitted lifecycle event: %+v", notes)
			}
		})
	}
}

func TestTenantComponentUpgradeEvents(t *testing.T) {
	before := &v1.ComponentStatus{Phase: v1.ComponentPhase_COMPONENT_PHASE_READY, TargetVersion: "1", InstalledVersion: "1", ObservedGeneration: 1}
	installing := &v1.ComponentStatus{Phase: v1.ComponentPhase_COMPONENT_PHASE_INSTALLING, TargetVersion: "2", InstalledVersion: "1", ObservedGeneration: 2}
	after := &v1.ComponentStatus{Phase: v1.ComponentPhase_COMPONENT_PHASE_READY, TargetVersion: "2", InstalledVersion: "2", ObservedGeneration: 2}
	if notes := tenantComponentEvents("mesh", before, installing); len(notes) != 1 || notes[0].Reason != "ComponentUpgrading" {
		t.Fatalf("upgrade start: %+v", notes)
	}
	if notes := tenantComponentEvents("mesh", installing, after); len(notes) != 1 || notes[0].Reason != "ComponentUpgraded" {
		t.Fatalf("upgrade finish: %+v", notes)
	}
	if notes := tenantComponentEvents("mesh", after, proto.CloneOf(after)); len(notes) != 0 {
		t.Fatalf("repeated component event: %+v", notes)
	}
}

func TestPlatformComponentUpgradeCompletionUsesInstalledVersion(t *testing.T) {
	before := &v1.PlatformComponent{Name: "mesh", Version: "2", InstalledVersion: "1", Phase: v1.ComponentPhase_COMPONENT_PHASE_INSTALLING}
	after := &v1.PlatformComponent{Name: "mesh", Version: "2", InstalledVersion: "2", Phase: v1.ComponentPhase_COMPONENT_PHASE_READY}
	notes := platformEvents(&v1.ClusterStatus{PlatformComponents: []*v1.PlatformComponent{before}}, &v1.ClusterStatus{PlatformComponents: []*v1.PlatformComponent{after}})
	if len(notes) != 1 || notes[0].Reason != "PlatformComponentUpgraded" {
		t.Fatalf("upgrade completion: %+v", notes)
	}
}
