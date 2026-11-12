package network

import (
	"testing"

	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Whether a network is imported decides if it may ever be rendered from
// its spec: a status with a field this build does not know still says so.
func TestStoredImportSurvivesUnknownFields(t *testing.T) {
	row := &model.Network{Resource: model.Resource{
		Status: model.JSON(`{"import":{"phase":"IMPORT_PHASE_CONVERGED"},"aFieldFromAnotherVersion":{"x":1}}`)}}
	if imp := storedImport(row); imp.GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_CONVERGED {
		t.Fatalf("storedImport = %v, want the import plan", imp)
	}
	if storedImport(&model.Network{}) != nil {
		t.Error("a created network has an import plan")
	}
}

// Ready resources are not READY while a provider has not looked at the
// latest change.
func TestReadyNeedsTheProvidersToHaveCaughtUp(t *testing.T) {
	view := func(generation, observed int64) *cloudv1.ManagedResource {
		return &cloudv1.ManagedResource{Kind: "Group", Name: "g", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY,
			Synced: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Generation: generation, ObservedGeneration: observed}
	}
	row := &model.Network{Resource: model.Resource{State: stateUpdating, Generation: 2, AppliedGeneration: 2}}
	for _, tt := range []struct {
		name string
		row  *model.Network
		view *cloudv1.ManagedResource
		want cloudv1.NetworkState
	}{
		{"caught up", row, view(3, 3), cloudv1.NetworkState_NETWORK_STATE_READY},
		{"change not seen yet", row, view(3, 2), cloudv1.NetworkState_NETWORK_STATE_UPDATING},
		{"generation not applied yet", &model.Network{Resource: model.Resource{State: stateUpdating, Generation: 2, AppliedGeneration: 1}},
			view(3, 3), cloudv1.NetworkState_NETWORK_STATE_UPDATING},
	} {
		if got := status(tt.row, []*cloudv1.ManagedResource{tt.view}, nil, nil).GetState(); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
}

// A label update that failed half-way is finished by the loop, and shared
// log groups are not the network's to tag.
func TestLoopFinishesTaggingAnImportedNetwork(t *testing.T) {
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: vpcWithFlowLogs()} // its log group is shared with other VPCs
	f.svc.deps.AWS.StaticKeys = testKeys
	ctx := t.Context()
	res, err := f.svc.ImportNetwork(ctx, importRequest(f.accountID))
	if err != nil {
		t.Fatal(err)
	}
	n := res.GetNetwork()
	if _, err := f.svc.TakeOverNetwork(ctx, &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: n.GetId(),
		AcknowledgeNotImported: true}); err != nil {
		t.Fatal(err)
	}
	// The labels were stored, the tagging did not happen (it failed).
	row, err := f.store.Networks().Get(ctx, tenant, n.GetId())
	if err != nil {
		t.Fatal(err)
	}
	row.Labels = model.Labels{"team": "net"}
	row.Generation++
	if err := f.store.Networks().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Reconcile(ctx, tenant, n.GetId()); err != nil {
		t.Fatal(err)
	}
	vpc, group := &awsec2.VPC{}, &awscwl.Group{}
	if err := f.kube.Get(ctx, key(n, "vpc"), vpc); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Get(ctx, key(n, "flow-logs"), group); err != nil {
		t.Fatal(err)
	}
	if vpc.Spec.ForProvider.Tags["team"] == nil || *vpc.Spec.ForProvider.Tags["team"] != "net" {
		t.Errorf("VPC tags after the loop's pass = %v, want the label", vpc.Spec.ForProvider.Tags)
	}
	if group.Spec.ForProvider.Tags["team"] != nil {
		t.Errorf("a log group shared with other VPCs was tagged: %v", group.Spec.ForProvider.Tags)
	}
	got, err := f.store.Networks().Get(ctx, tenant, n.GetId())
	if err != nil || got.AppliedGeneration != got.Generation {
		t.Errorf("applied generation %d of %d (%v)", got.AppliedGeneration, got.Generation, err)
	}
	if vpcs := f.count(t, &awsec2.VPCList{}, n.GetId()); vpcs != 1 {
		t.Errorf("%d VPCs: the imported network was rendered again", vpcs)
	}
}
