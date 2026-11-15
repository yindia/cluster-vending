package cluster

import (
	"strings"
	"testing"
	"time"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Phases, short.
const (
	noUpgrade      = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_UNSPECIFIED
	controlPlane   = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE
	nodesAndAddons = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS
)

// view is a managed resource's view for the upgrade status.
func upgradeView(kind, name string, state cloudv1.ManagedResourceState, synced cloudv1.ConditionStatus, message string) *cloudv1.ManagedResource {
	return &cloudv1.ManagedResource{Kind: kind, Name: name, State: state, Synced: synced, Message: message,
		Generation: 2, ObservedGeneration: 2}
}

func TestUpgradeStatus(t *testing.T) {
	const (
		ready   = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
		working = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING
		failed  = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED
		yes     = cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
		no      = cloudv1.ConditionStatus_CONDITION_STATUS_FALSE
	)
	asked := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	started := timestamppb.New(asked.Add(-time.Hour))
	row := func(state string, applied int64) *model.Cluster {
		return &model.Cluster{Resource: model.Resource{Base: model.Base{ID: "c-1", UpdatedAt: asked}, State: state, Generation: 3, AppliedGeneration: applied}}
	}
	settled := []*cloudv1.ManagedResource{
		upgradeView("Cluster", "prod", ready, yes, ""), upgradeView(KindNodeGroup, "prod-default", ready, yes, ""),
		upgradeView("Addon", "prod-addon-coredns", ready, yes, ""),
	}
	rolling := []*cloudv1.ManagedResource{
		upgradeView("Cluster", "prod", ready, yes, ""), upgradeView(KindNodeGroup, "prod-default", working, yes, ""),
		upgradeView("Addon", "prod-addon-coredns", ready, yes, ""),
	}
	at := func(step cloudv1.ClusterUpgradePhase, applied bool) *cloudv1.ClusterUpgrade {
		return &cloudv1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Phase: step, StartTime: started, Applied: applied}
	}
	onTarget := upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.35"},
		nodeGroups: map[string]string{"prod-default": "1.35"}, nodeSpecs: map[string]string{"prod-default": "1.35"}}
	allReady := componentsState{platformReady: true, tenantReady: true}

	for _, tt := range []struct {
		name    string
		u       upgrade
		row     *model.Cluster
		views   []*cloudv1.ManagedResource
		stored  *cloudv1.ClusterUpgrade
		comps   componentsState
		phase   cloudv1.ClusterUpgradePhase
		from    string
		start   *timestamppb.Timestamp
		message string
	}{
		{name: "no upgrade", u: onTarget, row: row(StateRunning, 3), views: settled, comps: allReady, phase: noUpgrade},
		{name: "control plane upgrading, first seen", u: upgrade{target: "1.35", controlPlane: "1.34", nodes: []string{"1.34"}},
			row: row(StateUpdating, 2), views: settled, phase: controlPlane, from: "1.34", start: timestamppb.New(asked),
			message: "upgrading the control plane from 1.34 to 1.35"},
		{name: "control plane upgrading, seen before", u: upgrade{target: "1.35", controlPlane: "1.34", nodes: []string{"1.34"}},
			row: row(StateUpdating, 2), views: settled, stored: at(controlPlane, true), phase: controlPlane, from: "1.34", start: started,
			message: "upgrading the control plane from 1.34 to 1.35"},
		{name: "control plane upgrade refused by the cloud", u: upgrade{target: "1.35", controlPlane: "1.34"},
			row: row(StateUpdating, 2), stored: at(controlPlane, true), phase: controlPlane, from: "1.34", start: started,
			views:   []*cloudv1.ManagedResource{upgradeView("Cluster", "prod", ready, no, "InvalidParameterException: unsupported version")},
			message: "Cluster prod: InvalidParameterException: unsupported version"},
		{name: "a stored upgrade to another version does not carry over", u: upgrade{target: "1.36", controlPlane: "1.35"},
			row: row(StateUpdating, 2), views: settled, stored: at(controlPlane, true), phase: controlPlane, from: "1.35",
			start: timestamppb.New(asked), message: "from 1.35 to 1.36"},
		{name: "the control plane is done: the node groups' turn",
			u:   upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.34"}, nodeGroups: map[string]string{"prod-default": "1.34"}},
			row: row(StateUpdating, 2), views: rolling, stored: at(controlPlane, true), phase: stepNodes, from: "1.34", start: started,
			message: "the control plane runs 1.35; node group prod-default 1.34 → 1.35"},
		{name: "a node group fails to follow", u: upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.34"}},
			row: row(StateUpdating, 2), stored: at(stepNodes, true), phase: stepNodes, from: "1.34", start: started,
			views: []*cloudv1.ManagedResource{upgradeView("Cluster", "prod", ready, yes, ""),
				upgradeView(KindNodeGroup, "prod-default", failed, no, "PodEvictionFailure: reached max retries")},
			message: "NodeGroup prod-default: PodEvictionFailure: reached max retries"},
		{name: "node groups settled: the add-ons' turn", u: onTarget, row: row(StateUpdating, 2), views: settled,
			stored: at(stepNodes, true), phase: stepAddons, from: "1.34", start: started, message: "the node groups run 1.35"},
		{name: "add-ons still held (generation not applied)", u: onTarget, row: row(StateUpdating, 2), views: settled,
			stored: at(stepAddons, true), phase: stepAddons, from: "1.34", start: started},
		{name: "add-ons settled: the platform components' turn", u: onTarget, row: row(StateUpdating, 3), views: settled,
			stored: at(stepAddons, true), comps: allReady, phase: stepPlatform, from: "1.34", start: started,
			message: "the platform components move to 1.35"},
		{name: "a platform component still moving", u: onTarget, row: row(StateRunning, 3), views: settled,
			stored: at(stepPlatform, true), comps: componentsState{tenantReady: true, platformBehind: []string{"cluster-autoscaler"}},
			phase: stepPlatform, from: "1.34", start: started, message: "platform components moving to 1.35: cluster-autoscaler"},
		{name: "platform settled: the tenant components' turn", u: onTarget, row: row(StateRunning, 3), views: settled,
			stored: at(stepPlatform, true), comps: allReady, phase: stepTenant, from: "1.34", start: started},
		{name: "done: every step settled", u: onTarget, row: row(StateRunning, 3), views: settled,
			stored: at(stepTenant, true), comps: allReady, phase: noUpgrade},
		{name: "a deleting cluster has no upgrade", u: upgrade{target: "1.35", controlPlane: "1.34"},
			row: row(StateDeleting, 2), views: settled, phase: noUpgrade},
		{name: "an imported cluster has no upgrade", u: upgrade{target: "1.35", controlPlane: "1.34"},
			row: row(StateImported, 2), views: settled, phase: noUpgrade},
	} {
		got := tt.u.status(tt.row, tt.views, tt.stored, tt.comps)
		if tt.phase == noUpgrade {
			if got != nil {
				t.Errorf("%s: %v, want no upgrade", tt.name, got)
			}
			continue
		}
		if got.GetPhase() != tt.phase || got.GetFromVersion() != tt.from || got.GetToVersion() != tt.u.target ||
			!got.GetStartTime().AsTime().Equal(tt.start.AsTime()) || !strings.Contains(got.GetMessage(), tt.message) {
			t.Errorf("%s: %v, want %s from %s since %s with %q", tt.name, got, tt.phase, tt.from, tt.start.AsTime(), tt.message)
		}
	}
}

// The gate holds what a later step applies: the node groups while the
// control plane is behind, the add-ons until the upgrade reaches their
// step.
func TestGateFollowsTheUpgradeStep(t *testing.T) {
	eks := func(version string) *awseks.Cluster {
		c := &awseks.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ngNamespace, Name: "prod"}}
		c.Spec.ForProvider.Region = ptr.To("us-east-1")
		c.Status.AtProvider.Version = ptr.To(version)
		return c
	}
	nodes := func(version string) *awseks.NodeGroup {
		ng := liveNodeGroup("prod-default", "t3.small")
		ng.Status.AtProvider.Version = ptr.To(version)
		return ng
	}
	records := []model.ManagedResource{
		{Kind: "Cluster", APIVersion: "eks.aws.m.upbound.io/v1beta1", Namespace: ngNamespace, Name: "prod"},
		ngRecord("1", "prod-default"),
	}
	row := func(step cloudv1.ClusterUpgradePhase) *model.Cluster {
		r := &model.Cluster{Resource: model.Resource{Base: model.Base{ID: "c-1"}, State: StateUpdating, Generation: 2, AppliedGeneration: 1}}
		if step != noUpgrade {
			raw, err := Encode(&cloudv1.ClusterStatus{Upgrade: &cloudv1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Phase: step}})
			if err != nil {
				t.Fatal(err)
			}
			r.Status = raw
		}
		return r
	}
	for _, tt := range []struct {
		name          string
		plane, groups string
		step          cloudv1.ClusterUpgradePhase
		gated         bool
		nodesAt       string
	}{
		{"control plane behind", "1.34", "1.34", controlPlane, true, "1.34"},
		{"node groups' turn: add-ons held", "1.35", "1.34", stepNodes, true, ""},
		{"nodes behind, nothing recorded: add-ons held", "1.35", "1.34", noUpgrade, true, ""},
		{"add-ons' turn: nothing held", "1.35", "1.35", stepAddons, false, ""},
		{"platform components' turn: nothing held", "1.35", "1.35", stepPlatform, false, ""},
		{"all at the target, nothing recorded", "1.35", "1.35", noUpgrade, false, ""},
	} {
		s, _, _ := ngService(t, records, eks(tt.plane), nodes(tt.groups))
		plan := crossplaneaws.ClusterPlan{KubernetesVersion: "1.35"}
		gated, err := s.GateUpgrade(t.Context(), row(tt.step), &plan)
		if err != nil || gated != tt.gated || plan.NodeKubernetesVersion != tt.nodesAt {
			t.Errorf("%s: gated %t (%v), nodes rendered at %q; want gated %t, nodes at %q", tt.name, gated, err,
				plan.NodeKubernetesVersion, tt.gated, tt.nodesAt)
		}
	}
}

// A pass records the upgrade it sees with the cluster: the step, and the
// versions it moves between. While the control plane is behind, the node
// group is applied at the control plane's version.
func TestReconcileRecordsTheUpgrade(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil { // applies everything
		t.Fatal(err)
	}
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	reports := func(version string) {
		t.Helper()
		eks := &awseks.Cluster{}
		if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix}, eks); err != nil {
			t.Fatal(err)
		}
		eks.Status.AtProvider.Version = ptr.To(version)
		if err := w.kube.Update(ctx, eks); err != nil {
			t.Fatal(err)
		}
	}
	recorded := func() *cloudv1.ClusterUpgrade { return storedUpgrade(w.cluster(t)) }

	reports("1.34")
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.ControlPlane.KubernetesVersion = "1.35"
	if row.Spec, err = Encode(spec); err != nil {
		t.Fatal(err)
	}
	row.Generation++
	row.State = StateUpdating
	if err := w.store.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if u := recorded(); u.GetPhase() != controlPlane || u.GetFromVersion() != "1.34" || u.GetToVersion() != "1.35" || u.GetStartTime() == nil {
		t.Fatalf("upgrade recorded while the control plane is behind = %v, want CONTROL_PLANE from 1.34 to 1.35", u)
	}
	ng := &awseks.NodeGroup{}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix + "-default"}, ng); err != nil ||
		ptr.Deref(ng.Spec.ForProvider.Version, "") != "1.34" {
		t.Errorf("node group version during the control plane upgrade = %q (%v), want 1.34", ptr.Deref(ng.Spec.ForProvider.Version, ""), err)
	}
	if got := w.cluster(t); got.AppliedGeneration >= got.Generation {
		t.Error("the generation counts as applied while the upgrade is held")
	}
	started := recorded().GetStartTime().AsTime()

	reports("1.35")
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix + "-default"}, ng); err != nil ||
		ptr.Deref(ng.Spec.ForProvider.Version, "") != "1.35" {
		t.Errorf("node group version once the control plane runs the target = %q (%v), want 1.35", ptr.Deref(ng.Spec.ForProvider.Version, ""), err)
	}
	if u := recorded(); u.GetPhase() != stepNodes || u.GetFromVersion() != "1.34" || !u.GetStartTime().AsTime().Equal(started) {
		t.Errorf("upgrade recorded once the control plane runs the target = %v, want NODE_GROUPS from 1.34 since %s", u, started)
	}
}
