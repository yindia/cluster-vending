package network

import (
	"context"
	"errors"
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
)

// guard fails every write to the management cluster while it is on, and
// counts them: what a handler must never do.
type guard struct {
	on     bool
	writes []string
}

func (g *guard) deny(verb string) error {
	if !g.on {
		return nil
	}
	g.writes = append(g.writes, verb)
	return errors.New("a handler wrote to the management cluster")
}

// funcs are the client interceptors of the guard: every writing verb.
func (g *guard) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := g.deny("create"); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := g.deny("update"); err != nil {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := g.deny("patch"); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := g.deny("delete"); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
		DeleteAllOf: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteAllOfOption) error {
			if err := g.deny("deleteAllOf"); err != nil {
				return err
			}
			return c.DeleteAllOf(ctx, obj, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if err := g.deny("apply"); err != nil {
				return err
			}
			return c.Apply(ctx, obj, opts...)
		},
	}
}

// during runs a handler call with the guard on and fails the test if the
// call failed or wrote anything.
func (g *guard) during(t *testing.T, call string, fn func() error) {
	t.Helper()
	g.on, g.writes = true, nil
	err := fn()
	g.on = false
	if err != nil || len(g.writes) > 0 {
		t.Fatalf("%s: %v, writes to the management cluster %v; a handler only stores what was asked for", call, err, g.writes)
	}
}

// Every mutating call stores the desired state and returns: nothing is
// created, changed or deleted in the management cluster until the
// reconciler runs.
func TestHandlersWriteNothingToTheManagementCluster(t *testing.T) {
	g := &guard{}
	f := newFixture(t, g.funcs())
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}
	f.svc.deps.AWS.StaticKeys = testKeys
	ctx := t.Context()
	row := func(id string) (state string, applied, generation int64) {
		t.Helper()
		r, err := f.store.Networks().Get(ctx, tenant, id)
		if err != nil {
			t.Fatal(err)
		}
		return r.State, r.AppliedGeneration, r.Generation
	}

	// Create: stored PROVISIONING, not applied.
	var created *cloudv1.Network
	g.during(t, "CreateNetwork", func() error {
		res, err := f.svc.CreateNetwork(ctx, f.request("prod", nil))
		created = res.GetNetwork()
		return err
	})
	if state, applied, generation := row(created.GetId()); state != netreconcile.StateProvisioning || applied >= generation ||
		created.GetStatus().GetState() != cloudv1.NetworkState_NETWORK_STATE_PROVISIONING || f.count(t, &awsec2.VPCList{}, created.GetId()) != 0 {
		t.Fatalf("after CreateNetwork: row %s applied %d of %d, answer %s, %d VPCs; want it stored, PROVISIONING and not applied",
			state, applied, generation, created.GetStatus().GetState(), f.count(t, &awsec2.VPCList{}, created.GetId()))
	}
	n := f.attachGateway(t, created.GetId())
	if f.count(t, &awsec2.VPCList{}, n.GetId()) != 1 {
		t.Fatal("the reconciler did not apply the stored network")
	}

	// Update: labels and flow logs are stored, the generation is behind.
	g.during(t, "UpdateNetwork", func() error {
		_, err := f.svc.UpdateNetwork(ctx, &cloudv1.UpdateNetworkRequest{TenantId: tenant, UpdateMask: mask("labels", "spec.flow_logs"),
			Network: &cloudv1.Network{Id: n.GetId(), Labels: map[string]string{"team": "net"},
				Spec: &cloudv1.NetworkSpec{FlowLogs: &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true)}}}})
		return err
	})
	if _, applied, generation := row(n.GetId()); applied >= generation || f.count(t, &awsec2.FlowLogList{}, n.GetId()) != 0 {
		t.Fatalf("after UpdateNetwork: applied %d of %d, %d flow logs; want the change stored and not applied",
			applied, generation, f.count(t, &awsec2.FlowLogList{}, n.GetId()))
	}
	f.reconcile(t, n.GetId())
	if f.count(t, &awsec2.FlowLogList{}, n.GetId()) != 1 {
		t.Error("the reconciler did not apply the stored update")
	}

	// Delete: stored DELETING, nothing deleted.
	g.during(t, "DeleteNetwork", func() error {
		_, err := f.svc.DeleteNetwork(ctx, &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: n.GetId()})
		return err
	})
	if state, _, _ := row(n.GetId()); state != netreconcile.StateDeleting || f.count(t, &awsec2.FlowLogList{}, n.GetId()) != 1 {
		t.Fatalf("after DeleteNetwork: row %s, %d flow logs; want DELETING and everything still there", state, f.count(t, &awsec2.FlowLogList{}, n.GetId()))
	}

	// Import: the plan is in the answer, the objects are not created.
	var imported *cloudv1.Network
	g.during(t, "ImportNetwork", func() error {
		res, err := f.svc.ImportNetwork(ctx, importRequest(f.accountID))
		imported = res.GetNetwork()
		return err
	})
	if st := imported.GetStatus(); st.GetState() != cloudv1.NetworkState_NETWORK_STATE_IMPORTED || len(st.GetImport().GetResources()) == 0 ||
		f.count(t, &awsec2.VPCList{}, imported.GetId()) != 0 {
		t.Fatalf("after ImportNetwork: %s, %d plan lines, %d VPCs; want the plan in the answer and nothing applied",
			st.GetState(), len(st.GetImport().GetResources()), f.count(t, &awsec2.VPCList{}, imported.GetId()))
	}
	f.reconcile(t, imported.GetId())
	vpc := &awsec2.VPC{}
	if err := f.kube.Get(ctx, key(imported, "vpc"), vpc); err != nil || vpc.Spec.ManagementPolicies[0] != "Observe" {
		t.Fatalf("the reconciler did not create the import's Observe-only VPC: %v (%v)", vpc.Spec.ManagementPolicies, err)
	}

	// TakeOver: stored, nothing owned.
	g.during(t, "TakeOverNetwork", func() error {
		_, err := f.svc.TakeOverNetwork(ctx, &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: imported.GetId(),
			AcknowledgeNotImported: true})
		return err
	})
	if err := f.kube.Get(ctx, key(imported, "vpc"), vpc); err != nil || vpc.Spec.ManagementPolicies[0] != "Observe" {
		t.Fatalf("after TakeOverNetwork: VPC policies %v (%v); want it not owned yet", vpc.Spec.ManagementPolicies, err)
	}
	f.reconcile(t, imported.GetId())
	if err := f.kube.Get(ctx, key(imported, "vpc"), vpc); err != nil || vpc.Spec.ManagementPolicies[0] != "*" {
		t.Errorf("the reconciler did not own the taken-over VPC: %v (%v)", vpc.Spec.ManagementPolicies, err)
	}
}

// The reconciler renders an import's objects from what the call stored:
// exactly what the plan would have applied, without another lookup.
func TestImportIsAppliedFromWhatWasStored(t *testing.T) {
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: vpcWithFlowLogs()}
	f.svc.deps.AWS.StaticKeys = testKeys
	ctx := t.Context()
	res, err := f.svc.ImportNetwork(ctx, importRequest(f.accountID))
	if err != nil {
		t.Fatal(err)
	}
	id := res.GetNetwork().GetId()
	// No discovery from here on: a second lookup would fail the pass.
	f.svc.deps.AWSDiscovery = fakeDiscoverer{err: errors.New("the reconciler looked the cloud up again")}
	f.reconcile(t, id)
	n := f.status(t, id)
	if got := len(n.GetStatus().GetManagedResources()); got != 24 {
		t.Fatalf("%d managed resources from the stored import, want 24", got)
	}
	// The same objects as rendering the plan directly.
	row, err := f.store.Networks().Get(ctx, tenant, id)
	if err != nil || row.AppliedGeneration != row.Generation {
		t.Fatalf("applied %d of %d (%v)", row.AppliedGeneration, row.Generation, err)
	}
	for _, v := range n.GetStatus().GetManagedResources() {
		if v.GetExternalName() == "" {
			t.Errorf("%s %s has no external name", v.GetKind(), v.GetName())
		}
	}
	flowLog := &awsec2.FlowLog{}
	if err := f.kube.Get(ctx, key(n, "flow-log-2"), flowLog); err != nil || *flowLog.Spec.ForProvider.TrafficType != "REJECT" ||
		flowLog.Annotations[crossplaneaws.ExternalNameAnnotation] == "" {
		t.Errorf("flow log from the stored import = %+v (%v)", flowLog.Spec.ForProvider, err)
	}
	// An update of the taken-over network keeps what was stored.
	if _, err := f.svc.TakeOverNetwork(ctx, &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: id, AcknowledgeNotImported: true}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, id)
	before := row.Input
	if _, err := f.svc.UpdateNetwork(ctx, &cloudv1.UpdateNetworkRequest{TenantId: tenant,
		Network: &cloudv1.Network{Id: id, DisplayName: "Imported"}, UpdateMask: mask("display_name")}); err != nil {
		t.Fatal(err)
	}
	if row, err = f.store.Networks().Get(ctx, tenant, id); err != nil {
		t.Fatal(err)
	}
	kept, err := netreconcile.KeepImportedResources(before, nil)
	if err != nil || len(kept) == 0 {
		t.Fatalf("the import stored nothing to render from: %v", err)
	}
	again, err := netreconcile.KeepImportedResources(row.Input, nil)
	if err != nil || string(again) != string(kept) {
		t.Errorf("the imported resources changed with an update:\n%s\n%s (%v)", kept, again, err)
	}
}
