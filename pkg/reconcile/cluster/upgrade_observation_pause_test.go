package cluster

import (
	"strings"
	"testing"
	"time"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func pendingPauseUpgrade(t *testing.T, events *capturedEvents) (world, *awseks.Cluster) {
	t.Helper()
	w := newWorld(t, interceptor.Funcs{}, nil, func(d *reconcile.Deps) { d.Events = events })
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	eks := &awseks.Cluster{}
	key := client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)}
	if err := w.kube.Get(t.Context(), key, eks); err != nil {
		t.Fatal(err)
	}
	eks.Status.AtProvider.Version = ptr.To("1.34")
	if err := w.kube.Update(t.Context(), eks); err != nil {
		t.Fatal(err)
	}
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.ControlPlane.KubernetesVersion = "1.35"
	row.Spec, err = Encode(spec)
	if err != nil {
		t.Fatal(err)
	}
	row.Generation++
	row.ResolvedGeneration = row.Generation
	row.State = StateUpdating
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	events.reasons, events.messages = nil, nil
	return w, eks
}

func TestUpgradePauseFreezeScopes(t *testing.T) {
	for _, scope := range []string{"platform", "tenant", "network", "cluster"} {
		for _, active := range []bool{false, true} {
			name := "before apply"
			if active {
				name = "active upgrade"
			}
			t.Run(scope+"/"+name, func(t *testing.T) {
				events := &capturedEvents{}
				w, _ := pendingPauseUpgrade(t, events)
				if active {
					if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
						t.Fatal(err)
					}
				}
				applied := w.cluster(t).AppliedGeneration
				before := storedUpgrade(w.cluster(t))
				hold := &cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: reconcile.FreezeName}, Spec: cloud.ReleaseHoldSpec{Platform: true, Scoped: scope != "platform", By: "operator", Reason: "maintenance"}}
				rule := cloud.FreezeRule{Frozen: true, Reason: "maintenance", By: "operator"}
				switch scope {
				case "tenant":
					rule.TenantID = worldTenant
				case "network":
					rule.NetworkID = w.row.NetworkID
				case "cluster":
					rule.ClusterID = w.row.ID
				}
				if scope != "platform" {
					hold.Spec.Rules = []cloud.FreezeRule{rule}
				}
				if err := w.kube.Create(t.Context(), hold); err != nil {
					t.Fatal(err)
				}
				events.reasons, events.messages = nil, nil
				var first *v1.Condition
				for pass := range 3 {
					// A restart must not replay the event: the dedupe marker is persisted.
					if pass == 2 {
						w.svc = New(w.svc.deps)
					}
					if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
						t.Fatal(err)
					}
					row := w.cluster(t)
					st := storedStatus(row)
					paused := upgradePauseCondition(st)
					if paused == nil || !strings.Contains(st.Message, "upgrade from 1.34 to 1.35 paused") || !strings.Contains(st.Message, scope) || !strings.Contains(st.Upgrade.Message, "maintenance") {
						t.Fatalf("missing paused status: %v", st)
					}
					if st.Upgrade.Halted || st.State == v1.ClusterState_CLUSTER_STATE_FAILED || row.AppliedGeneration != applied {
						t.Fatal("pause failed or advanced upgrade")
					}
					if first == nil {
						first = paused
					} else if !proto.Equal(first, paused) {
						t.Fatal("unchanged pause moved transition")
					}
					if active && (st.Upgrade.Phase != before.Phase || !proto.Equal(st.Upgrade.PhaseStartTime, before.PhaseStartTime)) {
						t.Fatal("pause advanced upgrade phase or timer")
					}
				}
				if events.count(ConditionUpgradePaused) != 1 || events.count(reconcile.ReasonUpgradeStarted) != 0 || !events.said(ConditionUpgradePaused, "maintenance") {
					t.Fatalf("pause events: %v %v", events.reasons, events.messages)
				}
				if err := w.kube.Delete(t.Context(), hold); err != nil {
					t.Fatal(err)
				}
				if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
					t.Fatal(err)
				}
				st := storedStatus(w.cluster(t))
				if upgradePauseCondition(st) != nil || strings.Contains(st.Message, "paused:") || strings.Contains(st.GetUpgrade().GetMessage(), "paused:") {
					t.Fatalf("resume retained pause: %v", st)
				}
				if events.count("UpgradeResumed") != 1 {
					t.Fatal(events.reasons)
				}
			})
		}
	}
}

func TestUpgradePauseManualProvider(t *testing.T) {
	events := &capturedEvents{}
	w, eks := pendingPauseUpgrade(t, events)
	eks.Annotations = map[string]string{managed.PausedAnnotation: "true"}
	if err := w.kube.Update(t.Context(), eks); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
			t.Fatal(err)
		}
	}
	st := storedStatus(w.cluster(t))
	if upgradePauseCondition(st) == nil || !strings.Contains(st.Message, "crossplane.io/paused") || events.count(ConditionUpgradePaused) != 1 {
		t.Fatalf("manual pause not reported once: %v %v", st, events.reasons)
	}
	if err := w.kube.Get(t.Context(), client.ObjectKeyFromObject(eks), eks); err != nil {
		t.Fatal(err)
	}
	delete(eks.Annotations, managed.PausedAnnotation)
	if err := w.kube.Update(t.Context(), eks); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if upgradePauseCondition(storedStatus(w.cluster(t))) != nil || events.count("UpgradeResumed") != 1 {
		t.Fatal("manual resume retained pause")
	}
}

func TestUpgradePauseIdleClusterHasNoUpgradeEvent(t *testing.T) {
	events := &capturedEvents{}
	w, eks := pendingPauseUpgrade(t, events)
	eks.Status.AtProvider.Version = ptr.To("1.35")
	if err := w.kube.Update(t.Context(), eks); err != nil {
		t.Fatal(err)
	}
	w.pause(t, alice)
	for range 2 {
		if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
			t.Fatal(err)
		}
	}
	if st := storedStatus(w.cluster(t)); st.Upgrade != nil || upgradePauseCondition(st) != nil {
		t.Fatalf("idle cluster acquired upgrade: %v", st)
	}
	if events.count(ConditionUpgradePaused) != 0 || events.count(reconcile.ReasonUpgradeStarted) != 0 {
		t.Fatal(events.reasons)
	}
}

func TestUpgradePauseDoesNotTriggerAddonRecovery(t *testing.T) {
	at := time.Now()
	oldNow := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = oldNow })
	previous := &v1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Phase: stepAddonsPhase, Applied: true, PhaseStartTime: timestamppb.New(at.Add(-24 * time.Hour)), StartTime: timestamppb.New(at.Add(-25 * time.Hour))}
	row := &model.Cluster{Resource: model.Resource{State: StateUpdating, Generation: 2, AppliedGeneration: 2, Frozen: alice}}
	row.Status, _ = Encode(&v1.ClusterStatus{Upgrade: previous, Conditions: []*v1.Condition{{Type: ConditionUpgradePaused, Status: v1.ConditionStatus_CONDITION_STATUS_TRUE, LastTransitionTime: timestamppb.New(at.Add(-24*time.Hour + time.Minute))}}})
	views := []*v1.ManagedResource{{Kind: "Addon", Name: "coredns", Paused: true, State: v1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING}}
	u := upgrade{target: "1.35", controlPlane: "1.35"}
	paused := u.status(row, views, previous, componentsState{})
	if paused.Halted || paused.Recoveries != 0 || !proto.Equal(paused.PhaseStartTime, previous.PhaseStartTime) {
		t.Fatal("pause triggered recovery or moved its clock")
	}
	row.Frozen = nil
	views[0].Paused = false
	resumed := u.status(row, views, previous, componentsState{})
	if resumed.Halted || resumed.Recoveries != 0 || at.Sub(resumed.PhaseStartTime.AsTime()) != time.Minute {
		t.Fatalf("paused time counted as a failure: %v", resumed)
	}
	if !proto.Equal(resumed.StartTime, previous.StartTime) {
		t.Fatal("resume reset overall start time")
	}
}

func TestUpgradePauseOnlyCurrentStep(t *testing.T) {
	row := &model.Cluster{}
	for _, test := range []struct {
		phase v1.ClusterUpgradePhase
		kind  string
		want  bool
	}{
		{stepControlPlane, "Cluster", true}, {stepControlPlane, "NodeGroup", false},
		{stepNodeGroups, "NodeGroup", true}, {stepNodeGroups, "Addon", false},
		{stepAddonsPhase, "Addon", true}, {stepAddonsPhase, "Cluster", false},
		{stepPlatformComps, "Release", true}, {stepTenantComps, "Release", true},
	} {
		upgrade := &v1.ClusterUpgrade{Phase: test.phase}
		views := []*v1.ManagedResource{{Kind: test.kind, Name: "manual", Paused: true}}
		if got := upgradePauseReason(row, upgrade, views) != ""; got != test.want {
			t.Fatalf("phase %v kind %s paused %v", test.phase, test.kind, got)
		}
	}
}

func TestUpgradePauseComponentInventory(t *testing.T) {
	for _, tenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "platform", true: "tenant"}[tenant], func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{}, nil)
			owner, id, phase := model.OwnerClusterComponents, w.row.ID, stepPlatformComps
			if tenant {
				component := &model.Component{TenantID: worldTenant, ClusterID: w.row.ID, Component: "policies", State: "COMPONENT_PHASE_INSTALLING"}
				if err := w.store.Components().Create(t.Context(), component); err != nil {
					t.Fatal(err)
				}
				owner, id, phase = model.OwnerComponent, component.ID, stepTenantComps
			}
			release := &unstructured.Unstructured{}
			release.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
			release.SetNamespace(worldNS)
			release.SetName("paused-component")
			release.SetAnnotations(map[string]string{managed.PausedAnnotation: "true"})
			if err := w.kube.Create(t.Context(), release); err != nil {
				t.Fatal(err)
			}
			record := model.ManagedResource{TenantID: worldTenant, OwnerKind: owner, OwnerID: id, APIVersion: crossplaneaws.HelmReleaseGVK.GroupVersion().String(), Kind: "Release", Namespace: worldNS, Name: release.GetName()}
			if err := w.store.ManagedResources().Put(t.Context(), []model.ManagedResource{record}); err != nil {
				t.Fatal(err)
			}
			status := &v1.ClusterStatus{Upgrade: &v1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Phase: phase}}
			views, err := w.svc.upgradePauseViews(t.Context(), w.cluster(t), status)
			if err != nil {
				t.Fatal(err)
			}
			withUpgradePause(w.cluster(t), status, views)
			if upgradePauseCondition(status) == nil || !strings.Contains(status.Message, "Release paused-component") {
				t.Fatal(status)
			}
			if len(status.ManagedResources) != 0 {
				t.Fatal("component inventory duplicated into core inventory")
			}
		})
	}
}

func TestUpgradePauseDoesNotHideHaltedFailure(t *testing.T) {
	row := &model.Cluster{Resource: model.Resource{Frozen: alice}}
	status := &v1.ClusterStatus{Message: "add-on recovery limit reached; retry required", Upgrade: &v1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Phase: stepAddonsPhase, Halted: true, Message: "add-on recovery limit reached; retry required"}}
	withUpgradePause(row, status, nil)
	if upgradePauseCondition(status) != nil || !status.Upgrade.Halted || status.Upgrade.Message != "add-on recovery limit reached; retry required" {
		t.Fatal("pause hid an existing halted upgrade failure")
	}
}

func TestUpgradePauseEventTransitions(t *testing.T) {
	upgrade := &v1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Phase: stepControlPlane}
	before := &v1.ClusterStatus{Upgrade: upgrade, Conditions: []*v1.Condition{{Type: ConditionUpgradePaused, Status: v1.ConditionStatus_CONDITION_STATUS_TRUE, Message: "tenant maintenance"}}}
	unchanged := proto.CloneOf(before)
	if notes := upgradeEvents(before, unchanged); len(notes) != 0 {
		t.Fatal("unchanged pause emitted another event")
	}
	changed := proto.CloneOf(before)
	changed.Conditions[0].Message = "network maintenance"
	if notes := upgradeEvents(before, changed); len(notes) != 1 || notes[0].Reason != ConditionUpgradePaused || notes[0].Message != "network maintenance" {
		t.Fatal(notes)
	}
	completed := &v1.ClusterStatus{State: v1.ClusterState_CLUSTER_STATE_RUNNING}
	if notes := upgradeEvents(before, completed); len(notes) != 1 || notes[0].Reason != reconcile.ReasonUpgradeFinished {
		t.Fatal("completion lost on unpause", notes)
	}
	halted := &v1.ClusterStatus{Upgrade: proto.CloneOf(upgrade)}
	halted.Upgrade.Halted = true
	halted.Upgrade.Message = "retry required"
	if notes := upgradeEvents(before, halted); len(notes) != 1 || notes[0].Reason != reconcile.ReasonUpgradeHalted {
		t.Fatal("halted upgrade falsely resumed", notes)
	}
	progressed := &v1.ClusterStatus{Upgrade: proto.CloneOf(upgrade)}
	progressed.Upgrade.Phase = stepNodeGroups
	notes := upgradeEvents(before, progressed)
	if len(notes) != 2 || notes[0].Reason != "UpgradeResumed" || notes[1].Reason != reconcile.ReasonUpgradeNodes {
		t.Fatal("resume hid the next upgrade step", notes)
	}
}

func TestUpgradePauseRetainsProgressWhenProviderDisappears(t *testing.T) {
	previous := &v1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Phase: stepControlPlane, Applied: true}
	row := &model.Cluster{Resource: model.Resource{Frozen: alice, State: StateUpdating}}
	observed := upgrade{target: "1.35"}
	if status := observed.status(row, nil, previous, componentsState{}); !proto.Equal(status, previous) {
		t.Fatal("missing provider erased paused upgrade")
	}
}
