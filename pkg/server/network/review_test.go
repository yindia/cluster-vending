package network

import (
	"context"
	"errors"
	"testing"

	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
)

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

// A takeover that stops half-way leaves no read-only network with owned
// objects (deleting it would delete the VPC): the row is taken over first
// and the loop finishes the rest.
func TestTakeoverStoppedHalfWayIsFinishedByTheLoop(t *testing.T) {
	fail := true
	f := newFixture(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if fail && obj.GetObjectKind().GroupVersionKind().Kind == "Subnet" {
			return errors.New("the API server went away")
		}
		return c.Update(ctx, obj, opts...)
	}})
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}
	f.svc.deps.AWS.StaticKeys = testKeys
	ctx := t.Context()
	res, err := f.svc.ImportNetwork(ctx, importRequest(f.accountID))
	if err != nil {
		t.Fatal(err)
	}
	id := res.GetNetwork().GetId()
	if _, err := f.svc.TakeOverNetwork(ctx, &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: id,
		AcknowledgeNotImported: true}); err == nil {
		t.Fatal("the takeover succeeded although a subnet could not be updated")
	}
	row, err := f.store.Networks().Get(ctx, tenant, id)
	if err != nil || row.State == netreconcile.StateImported || row.AppliedGeneration >= row.Generation {
		t.Fatalf("state %s, applied %d of %d (%v); want a taken-over network that is not applied yet",
			row.State, row.AppliedGeneration, row.Generation, err)
	}
	fail = false
	if err := f.svc.Reconcile(ctx, tenant, id); err != nil {
		t.Fatal(err)
	}
	subnets := &awsec2.SubnetList{}
	if err := f.kube.List(ctx, subnets); err != nil || len(subnets.Items) == 0 {
		t.Fatalf("subnets: %d (%v)", len(subnets.Items), err)
	}
	for _, s := range subnets.Items {
		if len(s.Spec.ManagementPolicies) != 1 || s.Spec.ManagementPolicies[0] != "*" {
			t.Errorf("subnet %s policies after the loop's pass = %v, want owned", s.Name, s.Spec.ManagementPolicies)
		}
	}
	if row, err = f.store.Networks().Get(ctx, tenant, id); err != nil || row.AppliedGeneration != row.Generation {
		t.Errorf("applied %d of %d (%v)", row.AppliedGeneration, row.Generation, err)
	}
}

// What the loop applied while a call changed the network may be older
// than what the call applied: the network is applied again.
func TestLoopAppliesAgainWhatChangedWhileItApplied(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	n := f.create(t, "prod", nil)
	seen, err := f.store.Networks().Get(ctx, tenant, n.GetId()) // the row as the loop read it
	if err != nil {
		t.Fatal(err)
	}
	f.svc.rec().Recheck(ctx, seen)
	if same, _ := f.store.Networks().Get(ctx, tenant, n.GetId()); same.ETag != seen.ETag {
		t.Fatal("a network nobody changed was marked behind")
	}
	if _, err := f.svc.UpdateNetwork(ctx, &cloudv1.UpdateNetworkRequest{TenantId: tenant,
		Network: &cloudv1.Network{Id: n.GetId(), Labels: map[string]string{"team": "net"}}, UpdateMask: mask("labels")}); err != nil {
		t.Fatal(err)
	}
	f.svc.rec().Recheck(ctx, seen)
	got, err := f.store.Networks().Get(ctx, tenant, n.GetId())
	if err != nil || got.AppliedGeneration >= got.Generation {
		t.Fatalf("applied %d of %d (%v); want the network applied again", got.AppliedGeneration, got.Generation, err)
	}
	// Recording the applied generation for the row as it was does not fail
	// the caller, and does not hide the change either.
	if err := f.svc.rec().MarkApplied(ctx, seen); err != nil {
		t.Errorf("markApplied on a changed row: %v", err)
	}
	if got, _ = f.store.Networks().Get(ctx, tenant, n.GetId()); got.AppliedGeneration >= got.Generation {
		t.Error("a stale apply recorded the generation as applied")
	}
}
