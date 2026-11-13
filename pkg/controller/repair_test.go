package controller

import (
	"slices"
	"testing"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// A RUNNING cluster with a recorded object missing is looked at again
// soon, and a cluster is woken by its Helm objects.
func TestClusterWithAMissingObjectIsLookedAtAgainSoon(t *testing.T) {
	status := func(state cloudv1.ManagedResourceState) []byte {
		raw, err := reconcilecluster.Encode(&cloudv1.ClusterStatus{ManagedResources: []*cloudv1.ManagedResource{
			{Kind: "ProviderConfig", Name: "prod-helm", State: state}}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	tests := []struct {
		name   string
		status []byte
		want   time.Duration
	}{
		{name: "everything there", status: status(cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY), want: resyncDelay},
		{name: "an object is missing", status: status(cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING), want: failedDelay},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := &model.Cluster{Resource: model.Resource{State: reconcilecluster.StateRunning, Generation: 1, AppliedGeneration: 1, Status: tt.status}}

			got := nextCluster(row, nil)

			if got.RequeueAfter != tt.want {
				t.Errorf("looked at again after %s, want %s", got.RequeueAfter, tt.want)
			}
		})
	}
	kinds := clusterHelmKinds()
	if !slices.Contains(kinds, crossplaneaws.HelmProviderConfigGVK) || !slices.Contains(kinds, crossplaneaws.HelmReleaseGVK) {
		t.Errorf("a cluster is not woken by its Helm ProviderConfig and Releases: %v", kinds)
	}
}

// A network applied in full gets back an object that was deleted from the
// management cluster when it can be created again without a second cloud
// resource (its flow logs' log group, named by the platform); one whose
// ID is AWS's (a subnet) stays missing, and then nothing is created.
func TestNetworkRecreatesADeletedObject(t *testing.T) {
	tests := []struct {
		name string
		// remove deletes objects of the network and returns the list to
		// count what must be back.
		remove      []client.ObjectList
		wantGroups  int
		wantSubnets int // fewer than before: not created again
	}{
		{name: "the flow logs' log group", remove: []client.ObjectList{&awscwl.GroupList{}}, wantGroups: 1},
		{name: "a subnet", remove: []client.ObjectList{&awsec2.SubnetList{}}, wantGroups: 1, wantSubnets: -1},
		{name: "a subnet and the log group", remove: []client.ObjectList{&awsec2.SubnetList{}, &awscwl.GroupList{}}, wantGroups: 0, wantSubnets: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange: a network with flow logs, applied in full.
			w := newWorld(t)
			ctx := t.Context()
			obj := handMadeNetwork("prod", "aws-444455556666")
			obj.Spec.FlowLogs = &cloudv1alpha1.NetworkFlowLogs{Enabled: ptr.To(true)}
			w.must(t, w.k.Create(ctx, obj))
			r := NewNetworkReconciler(w.deps)
			for range 2 { // adopted, then applied
				if _, err := r.Reconcile(ctx, request("prod")); err != nil {
					t.Fatal(err)
				}
			}
			id := w.network(t, "prod").Labels["cloud.cluster-vending.io/id"]
			gateways := &awsec2.InternetGatewayList{}
			w.must(t, w.k.List(ctx, gateways, client.MatchingLabels{crossplane.LabelNetwork: id}))
			gateways.Items[0].SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
			w.must(t, w.k.Status().Update(ctx, &gateways.Items[0]))
			if _, err := r.Reconcile(ctx, request("prod")); err != nil {
				t.Fatal(err)
			}
			row, err := w.store.Networks().Get(ctx, tenant, id)
			if err != nil || row.AppliedGeneration != row.Generation {
				t.Fatalf("applied %d of %d (%v)", row.AppliedGeneration, row.Generation, err)
			}
			subnets := w.count(t, &awsec2.SubnetList{}, crossplane.LabelNetwork, id)
			if w.count(t, &awscwl.GroupList{}, crossplane.LabelNetwork, id) != 1 || subnets == 0 {
				t.Fatal("the network has no log group or no subnets")
			}
			for _, list := range tt.remove {
				w.must(t, w.k.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{crossplane.LabelNetwork: id}))
				items, err := meta.ExtractList(list)
				w.must(t, err)
				w.must(t, w.k.Delete(ctx, items[0].(client.Object)))
			}

			// Act
			_, err = r.Reconcile(ctx, request("prod"))

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if got := w.count(t, &awscwl.GroupList{}, crossplane.LabelNetwork, id); got != tt.wantGroups {
				t.Errorf("%d log groups, want %d", got, tt.wantGroups)
			}
			if got := w.count(t, &awsec2.SubnetList{}, crossplane.LabelNetwork, id); got != subnets+tt.wantSubnets {
				t.Errorf("%d subnets, want %d", got, subnets+tt.wantSubnets)
			}
		})
	}
}
