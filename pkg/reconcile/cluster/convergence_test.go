package cluster

import (
	"context"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestStatusConvergence(t *testing.T) {
	const (
		ready  = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
		failed = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED
		synced = cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
	)
	mr := func(kind string, gen, observed int64) *cloudv1.ManagedResource {
		return &cloudv1.ManagedResource{Kind: kind, Name: kind, State: ready, Synced: synced, Generation: gen, ObservedGeneration: observed}
	}
	tests := []struct {
		name         string
		row          model.Cluster
		views        []*cloudv1.ManagedResource
		want         cloudv1.ClusterState
		wantObserved int64
	}{
		{"converged", model.Cluster{Resource: model.Resource{State: StateUpdating, Generation: 2, AppliedGeneration: 2, ObservedGeneration: 1}},
			[]*cloudv1.ManagedResource{mr("Cluster", 3, 3), mr("NodeGroup", 2, 2)}, cloudv1.ClusterState_CLUSTER_STATE_RUNNING, 2},
		{"provider behind the spec", model.Cluster{Resource: model.Resource{State: StateUpdating, Generation: 2, AppliedGeneration: 2, ObservedGeneration: 1}},
			[]*cloudv1.ManagedResource{mr("Cluster", 3, 2), mr("NodeGroup", 2, 2)}, cloudv1.ClusterState_CLUSTER_STATE_UPDATING, 1},
		// Some kinds (RolePolicyAttachment) carry no observedGeneration on
		// Synced at all: that is not a provider behind its spec.
		{"provider reports no observed generation", model.Cluster{Resource: model.Resource{State: StateUpdating, Generation: 2, AppliedGeneration: 2, ObservedGeneration: 1}},
			[]*cloudv1.ManagedResource{mr("Cluster", 3, 3), mr("RolePolicyAttachment", 2, 0)}, cloudv1.ClusterState_CLUSTER_STATE_RUNNING, 2},
		{"generation not fully applied", model.Cluster{Resource: model.Resource{State: StateUpdating, Generation: 2, AppliedGeneration: 1, ObservedGeneration: 1}},
			[]*cloudv1.ManagedResource{mr("Cluster", 3, 3)}, cloudv1.ClusterState_CLUSTER_STATE_UPDATING, 1},
		{"version skew during an upgrade", model.Cluster{Resource: model.Resource{State: StateUpdating, Generation: 2, AppliedGeneration: 1}},
			[]*cloudv1.ManagedResource{mr("Cluster", 3, 3), {Kind: "NodeGroup", Name: "ng", State: failed,
				Message: "InvalidParameterException: Requested Nodegroup release version 1.35 is newer than the cluster version 1.34"}},
			cloudv1.ClusterState_CLUSTER_STATE_UPDATING, 0},
		{"version skew outside an upgrade fails", model.Cluster{Resource: model.Resource{State: StateRunning, Generation: 2, AppliedGeneration: 2}},
			[]*cloudv1.ManagedResource{{Kind: "NodeGroup", Name: "ng", State: failed, Message: "newer than the cluster version"}},
			cloudv1.ClusterState_CLUSTER_STATE_FAILED, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := status(&tt.row, tt.views, false)
			if st.GetState() != tt.want || st.GetObservedGeneration() != tt.wantObserved {
				t.Errorf("state %s observed %d (%s), want %s observed %d", st.GetState(), st.GetObservedGeneration(),
					st.GetMessage(), tt.want, tt.wantObserved)
			}
		})
	}
}

func TestUpgradeRendersNodesAtTheControlPlaneVersion(t *testing.T) {
	eks := func(version string) *awseks.Cluster {
		c := &awseks.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ngNamespace, Name: "prod"}}
		c.Spec.ForProvider.Region = ptr.To("us-east-1")
		c.Status.AtProvider.Version = ptr.To(version)
		return c
	}
	coredns := &awseks.Addon{ObjectMeta: metav1.ObjectMeta{Namespace: ngNamespace, Name: "prod-addon-coredns"}}
	coredns.Spec.ForProvider.Region = ptr.To("us-east-1")
	coredns.Spec.ForProvider.AddonName = ptr.To("coredns")
	coredns.Spec.ForProvider.AddonVersion = ptr.To("v1.11.4-eksbuild.1")
	records := []model.ManagedResource{
		{Kind: "Cluster", APIVersion: "eks.aws.m.upbound.io/v1beta1", Namespace: ngNamespace, Name: "prod"},
		{Kind: "Addon", APIVersion: "eks.aws.m.upbound.io/v1beta1", Namespace: ngNamespace, Name: "prod-addon-coredns"},
	}
	row := &model.Cluster{Base: model.Base{ID: "c-1"}}
	target := func() crossplaneaws.ClusterPlan {
		return crossplaneaws.ClusterPlan{KubernetesVersion: "1.35",
			Addons: []crossplaneaws.AddonPlan{{Name: "coredns", Version: "v1.12.0-eksbuild.1"}, {Name: "kube-proxy", Version: "v1.35.0"}}}
	}

	// The control plane still runs 1.34: nodes at 1.34, coredns at its version.
	s, _, _ := ngService(t, records, []client.Object{eks("1.34"), coredns}...)
	plan := target()
	gated, err := s.GateUpgrade(context.Background(), row, &plan)
	if err != nil || !gated || plan.NodeKubernetesVersion != "1.34" || plan.Addons[0].Version != "v1.11.4-eksbuild.1" ||
		plan.Addons[1].Version != "v1.35.0" {
		t.Errorf("during the upgrade: gated %v (%v), nodes %q, add-ons %+v", gated, err, plan.NodeKubernetesVersion, plan.Addons)
	}

	// The control plane reports 1.35: everything at the target.
	s, _, _ = ngService(t, records, []client.Object{eks("1.35"), coredns}...)
	plan = target()
	gated, err = s.GateUpgrade(context.Background(), row, &plan)
	if err != nil || gated || plan.NodeKubernetesVersion != "" || plan.Addons[0].Version != "v1.12.0-eksbuild.1" {
		t.Errorf("after the upgrade: gated %v (%v), nodes %q, add-ons %+v", gated, err, plan.NodeKubernetesVersion, plan.Addons)
	}
}
