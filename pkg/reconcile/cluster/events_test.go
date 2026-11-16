package cluster

import (
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func TestStatusEvents(t *testing.T) {
	const (
		ready    = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
		creating = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING
	)
	mr := func(kind, name string, state cloudv1.ManagedResourceState) *cloudv1.ManagedResource {
		return &cloudv1.ManagedResource{Kind: kind, Name: name, State: state}
	}
	st := func(state cloudv1.ClusterState, message string, resources ...*cloudv1.ManagedResource) *cloudv1.ClusterStatus {
		return &cloudv1.ClusterStatus{State: state, Message: message, ManagedResources: resources}
	}
	pc := func(name string, phase cloudv1.ComponentPhase, recoveries int32) *cloudv1.PlatformComponent {
		return &cloudv1.PlatformComponent{Name: name, Version: "1.0.0", Phase: phase, Recoveries: recoveries,
			Message: "chart " + name + ": create failed", Chart: name, HelmState: "failed", Failures: 2}
	}
	withComponents := func(base *cloudv1.ClusterStatus, components ...*cloudv1.PlatformComponent) *cloudv1.ClusterStatus {
		base.PlatformComponents = components
		withPlatform(base, &platform{release: "r", components: components})
		return base
	}
	imported := func(phase cloudv1.ImportPhase, state cloudv1.ClusterState) *cloudv1.ClusterStatus {
		return &cloudv1.ClusterStatus{State: state, Import: &cloudv1.ImportStatus{Phase: phase,
			Resources: []*cloudv1.ImportedResource{{Disposition: cloudv1.Disposition_DISPOSITION_BLOCKING, Detail: "a custom launch template"}}}}
	}
	const (
		provisioning = cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING
		running      = cloudv1.ClusterState_CLUSTER_STATE_RUNNING
		updating     = cloudv1.ClusterState_CLUSTER_STATE_UPDATING
		failed       = cloudv1.ClusterState_CLUSTER_STATE_FAILED
		deleting     = cloudv1.ClusterState_CLUSTER_STATE_DELETING
		importedSt   = cloudv1.ClusterState_CLUSTER_STATE_IMPORTED
	)
	tests := []struct {
		name          string
		before, after *cloudv1.ClusterStatus
		want          []string // "Type Reason"
		message       string   // part of the last event's message
	}{
		{name: "nothing recorded, nothing found", before: &cloudv1.ClusterStatus{}, after: &cloudv1.ClusterStatus{}},
		{name: "the first pass", before: &cloudv1.ClusterStatus{}, after: st(provisioning, "0 of 3 resources ready"),
			want: []string{"Normal Provisioning"}},
		{name: "still provisioning", before: st(provisioning, "0 of 3 resources ready"), after: st(provisioning, "1 of 3 resources ready")},
		{name: "the EKS cluster becomes ACTIVE",
			before: st(provisioning, "", mr("Cluster", "c", creating)),
			after:  st(provisioning, "", mr("Cluster", "c", ready), mr("NodeGroup", "ng", creating)),
			want:   []string{"Normal ControlPlaneActive", "Normal NodeGroupCreating"}, message: "installed"},
		{name: "node group and add-ons ready, then running",
			before: st(provisioning, "", mr("Cluster", "c", ready), mr("NodeGroup", "ng", creating), mr("Addon", "a", creating)),
			after:  st(running, "", mr("Cluster", "c", ready), mr("NodeGroup", "ng", ready), mr("Addon", "a", ready)),
			want:   []string{"Normal NodeGroupReady", "Normal AddonsReady", "Normal NodeGroupCreated", "Normal AddonInstalled", "Normal Running"}, message: "all 3 resources are ready"},
		{name: "running stays running", before: st(running, ""), after: st(running, "")},
		{name: "a change is applied", before: st(running, ""), after: st(updating, "applying generation 3 (4 of 5 resources ready)"),
			want: []string{"Normal Updating"}, message: "generation 3"},
		{name: "a resource fails", before: st(running, ""), after: st(failed, "NodeGroup ng: InvalidParameterException: nope"),
			want: []string{"Warning Failed"}, message: "NodeGroup ng"},
		{name: "the same resource fails with another text", before: st(failed, "NodeGroup ng: error on port 1"),
			after: st(failed, "NodeGroup ng: error on port 2")},
		{name: "another resource fails", before: st(failed, "NodeGroup ng: x"), after: st(failed, "Addon a: y"),
			want: []string{"Warning Failed"}, message: "Addon a"},
		{name: "deletion starts", before: st(running, ""), after: st(deleting, "deleting; waiting for NodeGroup ng; 40 resources left"),
			want: []string{"Normal Deleting"}, message: "waiting for NodeGroup ng"},
		{name: "deletion goes on", before: st(deleting, "deleting; waiting for NodeGroup ng; 40 resources left"),
			after: st(deleting, "deleting; waiting for NodeGroup ng; 39 resources left")},
		{name: "deletion waits for something else", before: st(deleting, "deleting; waiting for NodeGroup ng; 40 resources left"),
			after: st(deleting, "deleting; waiting for Cluster c; 12 resources left"), want: []string{"Normal Deleting"}},
		{name: "deletion gets stuck, once", before: st(deleting, "deleting; 3 resources left"),
			after: st(deleting, "deleting; 3 resources left; stuck: Role r (DeleteFailed: x) for 5m0s"), want: []string{"Normal Deleting"}},
		{name: "deletion stays stuck", before: st(deleting, "deleting; 3 resources left; stuck: Role r (DeleteFailed: x) for 5m0s"),
			after: st(deleting, "deleting; 3 resources left; stuck: Role r (DeleteFailed: x) for 6m0s")},
		{name: "the pass fails", before: st(provisioning, ""),
			after: func() *cloudv1.ClusterStatus {
				s := st(provisioning, "")
				withFailure(s, &cloudv1.Condition{Type: ConditionReconciled, Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
					Reason: ReasonFailed, Message: "tenant acme has no network n"})
				return s
			}(), want: []string{"Warning Failed"}, message: "tenant acme has no network n"},
		{name: "an import is discovered", before: imported(cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING, importedSt),
			after: imported(cloudv1.ImportPhase_IMPORT_PHASE_PLANNED, importedSt), want: []string{"Normal ImportDiscovered"}},
		{name: "an import is blocked", before: imported(cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING, importedSt),
			after: imported(cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED, failed), want: []string{"Warning ImportBlocked"},
			message: "a custom launch template"},
		{name: "an import is taken over", before: imported(cloudv1.ImportPhase_IMPORT_PHASE_PLANNED, importedSt),
			after: imported(cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING, updating), want: []string{"Normal TakenOver"}},
		{name: "an upgrade starts", before: st(running, ""),
			after: func() *cloudv1.ClusterStatus {
				s := st(running, "")
				s.Upgrade = &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37"}
				return s
			}(), want: []string{"Normal UpgradeStarted"}, message: "from 1.36 to 1.37"},
		{name: "an upgrade finishes",
			before: func() *cloudv1.ClusterStatus {
				s := st(running, "")
				s.Upgrade = &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37"}
				return s
			}(), after: st(running, ""), want: []string{"Normal UpgradeFinished"}, message: "1.37"},
		{name: "a component starts installing", before: st(running, ""),
			after: withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, 0)),
			want:  []string{"Normal PlatformComponentInstalling"}, message: "mesh 1.0.0"},
		{name: "a component fails",
			before: withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, 0)),
			after:  withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, 0)),
			want:   []string{"Warning PlatformComponentFailed"}, message: "Helm state failed, failed 2 times"},
		{name: "a failed component stays failed",
			before: withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, 0)),
			after:  withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, 0))},
		{name: "the last component becomes ready",
			before: withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, 0)),
			after:  withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_READY, 0)),
			want:   []string{"Normal PlatformComponentReady", "Normal PlatformComponentsReady"}},
		{name: "a stuck release is recovered",
			before: withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, 0)),
			after:  withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, 1)),
			want:   []string{"Warning StuckReleaseRecovered"}, message: "recovery 1 of 3"},
		{name: "the last recovery",
			before: withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, 2)),
			after:  withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, 3)),
			want:   []string{"Warning StuckReleaseRecovered", "Warning RecoveryLimitReached"}},
		{name: "a component is removed",
			before: withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_READY, 0), pc("old", cloudv1.ComponentPhase_COMPONENT_PHASE_READY, 0)),
			after:  withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_READY, 0)),
			want:   []string{"Normal PlatformComponentDeleting", "Normal PlatformComponentRemoved"}, message: "old"},
		{name: "components report removal with a deleted cluster",
			before: withComponents(st(running, ""), pc("mesh", cloudv1.ComponentPhase_COMPONENT_PHASE_READY, 0)),
			after:  st(deleting, "deleting; 3 resources left"), want: []string{"Normal Deleting", "Normal PlatformComponentDeleting", "Normal PlatformComponentRemoved"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := statusEvents(tt.before, tt.after)

			var have []string
			for _, n := range got {
				have = append(have, n.Type+" "+n.Reason)
			}
			if !slices.Equal(have, tt.want) {
				t.Fatalf("events = %v, want %v", have, tt.want)
			}
			if tt.message != "" && !strings.Contains(got[len(got)-1].Message, tt.message) {
				t.Errorf("message %q lacks %q", got[len(got)-1].Message, tt.message)
			}
		})
	}
}

// A pass emits what changed once, keeps it with the status, and a second
// pass that finds the cluster as it was emits nothing.
func TestReconcileEmitsEventsOnceAndKeepsThem(t *testing.T) {
	events := &reconcile.RecordedEvents{}
	w := newWorld(t, interceptor.Funcs{}, nil, func(d *reconcile.Deps) { d.Events = events })

	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	first := events.All()
	if len(first) == 0 {
		t.Fatal("the first pass emitted nothing")
	}
	for _, e := range first {
		if e.Object != (reconcile.Object{Kind: "Cluster", Namespace: worldNS, Name: "prod"}) || e.Reason == "" || e.Message == "" {
			t.Errorf("event %+v is not about the cluster's object, or says nothing", e)
		}
	}
	recent := storedStatus(w.cluster(t)).GetRecentEvents()
	if len(recent) != len(first) || recent[len(recent)-1].GetReason() != first[len(first)-1].Reason ||
		recent[0].GetTime() == nil || recent[0].GetCount() != 1 {
		t.Fatalf("status keeps %v, emitted %v", recent, events.Reasons())
	}
	c, err := w.svc.View(t.Context(), w.cluster(t))
	if err != nil || len(c.GetStatus().GetRecentEvents()) != len(recent) {
		t.Fatalf("a read returns %d recent events, %v; want %d", len(c.GetStatus().GetRecentEvents()), err, len(recent))
	}

	events.Reset()
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if again := events.Reasons(); len(again) != 0 {
		t.Errorf("a pass that changed nothing emitted %v", again)
	}
}
