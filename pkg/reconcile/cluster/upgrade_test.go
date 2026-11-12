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

func TestUpgradePhase(t *testing.T) {
	for _, tt := range []struct {
		name      string
		u         upgrade
		following bool
		want      cloudv1.ClusterUpgradePhase
	}{
		{"no version in the spec", upgrade{controlPlane: "1.34"}, false, noUpgrade},
		{"no EKS cluster yet", upgrade{target: "1.35"}, false, noUpgrade},
		{"nothing to do", upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.35"}}, false, noUpgrade},
		{"the control plane is behind", upgrade{target: "1.35", controlPlane: "1.34", nodes: []string{"1.34"}}, false, controlPlane},
		{"the control plane is behind, also when following", upgrade{target: "1.35", controlPlane: "1.34"}, true, controlPlane},
		{"a node group is behind", upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.35", "1.34"}}, false, nodesAndAddons},
		{"nodes report the target, add-ons still roll out", upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.35"}}, true, nodesAndAddons},
		{"node groups that report nothing yet", upgrade{target: "1.35", controlPlane: "1.35"}, false, noUpgrade},
	} {
		if got := tt.u.phase(tt.following); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
}

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
	before := &cloudv1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Phase: controlPlane, StartTime: started}

	for _, tt := range []struct {
		name    string
		u       upgrade
		row     *model.Cluster
		views   []*cloudv1.ManagedResource
		stored  *cloudv1.ClusterUpgrade
		phase   cloudv1.ClusterUpgradePhase
		from    string
		start   *timestamppb.Timestamp
		message string
	}{
		{name: "no upgrade", u: upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.35"}},
			row: row(StateRunning, 3), views: settled, phase: noUpgrade},
		{name: "control plane upgrading, first seen", u: upgrade{target: "1.35", controlPlane: "1.34", nodes: []string{"1.34"}},
			row: row(StateUpdating, 2), views: settled, phase: controlPlane, from: "1.34", start: timestamppb.New(asked),
			message: "upgrading the control plane from 1.34 to 1.35"},
		{name: "control plane upgrading, seen before", u: upgrade{target: "1.35", controlPlane: "1.34", nodes: []string{"1.34"}},
			row: row(StateUpdating, 2), views: settled, stored: before, phase: controlPlane, from: "1.34", start: started,
			message: "upgrading the control plane from 1.34 to 1.35"},
		{name: "control plane upgrade refused by the cloud", u: upgrade{target: "1.35", controlPlane: "1.34"},
			row: row(StateUpdating, 2), stored: before, phase: controlPlane, from: "1.34", start: started,
			views:   []*cloudv1.ManagedResource{upgradeView("Cluster", "prod", ready, no, "InvalidParameterException: unsupported version")},
			message: "Cluster prod: InvalidParameterException: unsupported version"},
		{name: "a stored upgrade to another version does not carry over", u: upgrade{target: "1.36", controlPlane: "1.35"},
			row: row(StateUpdating, 2), views: settled, stored: before, phase: controlPlane, from: "1.35",
			start: timestamppb.New(asked), message: "from 1.35 to 1.36"},
		{name: "nodes behind the control plane", u: upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.34"}},
			row: row(StateUpdating, 3), views: rolling, stored: before, phase: nodesAndAddons, from: "1.34", start: started,
			message: "the control plane runs 1.35"},
		{name: "nodes report the target, the generation is not applied in full yet",
			u:   upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.35"}},
			row: row(StateUpdating, 2), views: settled, stored: before, phase: nodesAndAddons, from: "1.34", start: started,
			message: "the control plane runs 1.35"},
		{name: "node group fails to follow", u: upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.34"}},
			row: row(StateUpdating, 3), stored: before, phase: nodesAndAddons, from: "1.34", start: started,
			views: []*cloudv1.ManagedResource{upgradeView("Cluster", "prod", ready, yes, ""),
				upgradeView(KindNodeGroup, "prod-default", failed, no, "PodEvictionFailure: reached max retries")},
			message: "NodeGroup prod-default: PodEvictionFailure: reached max retries"},
		{name: "done: everything settled at the target", u: upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.35"}},
			row: row(StateUpdating, 3), views: settled, stored: before, phase: noUpgrade},
		{name: "a deleting cluster has no upgrade", u: upgrade{target: "1.35", controlPlane: "1.34"},
			row: row(StateDeleting, 2), views: settled, phase: noUpgrade},
		{name: "an imported cluster has no upgrade", u: upgrade{target: "1.35", controlPlane: "1.34"},
			row: row(StateImported, 2), views: settled, phase: noUpgrade},
	} {
		got := tt.u.status(tt.row, tt.views, tt.stored)
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

// The gate and the status read the same observation: while the status
// says CONTROL_PLANE the plan is held, and not otherwise.
func TestGateFollowsTheUpgradePhase(t *testing.T) {
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
	row := &model.Cluster{Resource: model.Resource{Base: model.Base{ID: "c-1"}, State: StateUpdating, Generation: 2, AppliedGeneration: 1}}
	for _, tt := range []struct {
		name          string
		plane, groups string
		phase         cloudv1.ClusterUpgradePhase
		gated         bool
	}{
		{"control plane behind", "1.34", "1.34", controlPlane, true},
		{"nodes behind", "1.35", "1.34", nodesAndAddons, false},
		{"all at the target", "1.35", "1.35", noUpgrade, false},
	} {
		s, _, _ := ngService(t, records, eks(tt.plane), nodes(tt.groups))
		u, err := s.observeUpgrade(t.Context(), records, "1.35")
		if err != nil {
			t.Fatal(err)
		}
		if got := u.status(row, nil, nil).GetPhase(); got != tt.phase {
			t.Errorf("%s: status phase %s, want %s", tt.name, got, tt.phase)
		}
		plan := crossplaneaws.ClusterPlan{KubernetesVersion: "1.35"}
		gated, err := s.GateUpgrade(t.Context(), row, &plan)
		if err != nil || gated != tt.gated || (gated && plan.NodeKubernetesVersion != tt.plane) {
			t.Errorf("%s: gated %t (%v), nodes rendered at %q; want gated %t", tt.name, gated, err, plan.NodeKubernetesVersion, tt.gated)
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
	if u := recorded(); u != nil && (u.GetPhase() != nodesAndAddons || u.GetFromVersion() != "1.34" || !u.GetStartTime().AsTime().Equal(started)) {
		t.Errorf("upgrade recorded once the control plane runs the target = %v, want none or NODES_AND_ADDONS from 1.34 since %s", u, started)
	}
}
