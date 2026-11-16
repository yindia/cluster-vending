package network

import (
	"context"
	"errors"
	"slices"
	"testing"

	"connectrpc.com/connect/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestNetworkPlanRemovalsAndStaleApply(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, obj runtimeApply, opts ...client.ApplyOption) error {
		ao := &client.ApplyOptions{}
		ao.ApplyOptions(opts)
		if !slices.Contains(ao.DryRun, metav1.DryRunAll) {
			return c.Apply(ctx, obj, opts...)
		}
		body, ok := obj.(client.Object)
		if !ok {
			return errors.New("expected an object apply")
		}
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(body.GetObjectKind().GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(body), live); err != nil {
			return err
		}
		live.SetResourceVersion("")
		scratch := fake.NewClientBuilder().WithScheme(c.Scheme()).WithObjects(live).Build()
		return scratch.Apply(ctx, obj, client.FieldOwner(ao.FieldManager), client.ForceOwnership)
	}})
	req := f.request("planned", nil)
	req.Network.Spec.FlowLogs = &v1.NetworkFlowLogs{Enabled: ptr.To(true)}
	created, err := f.svc.CreateNetwork(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	n := f.attachGateway(t, created.Network.Id)
	before, err := f.store.Networks().Get(t.Context(), tenant, n.Id)
	if err != nil {
		t.Fatal(err)
	}
	update := &v1.UpdateNetworkRequest{TenantId: tenant, Network: &v1.Network{Id: n.Id}, UpdateMask: mask("spec.flow_logs"), ValidateOnly: true}
	preview, err := f.svc.UpdateNetwork(t.Context(), update)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.GetPlan().GetApplicable() || preview.GetPlan().GetPlanId() == "" || preview.GetUpdateRecorded() {
		t.Fatalf("preview: %v", preview)
	}
	deleted := map[string]bool{}
	for _, change := range preview.Plan.Changes {
		if change.Action == v1.PlannedAction_PLANNED_ACTION_DELETE {
			deleted[change.Kind] = true
		}
	}
	for _, kind := range []string{"FlowLog", "Group", "Role", "Policy", "RolePolicyAttachment"} {
		if !deleted[kind] {
			t.Errorf("missing removal for %s: %v", kind, preview.Plan)
		}
	}
	after, err := f.store.Networks().Get(t.Context(), tenant, n.Id)
	if err != nil {
		t.Fatal(err)
	}
	if before.ETag != after.ETag || before.Generation != after.Generation || f.count(t, &awsec2.FlowLogList{}, n.Id) != 1 {
		t.Fatal("preview wrote state")
	}
	flow := &awsec2.FlowLog{}
	if err := f.kube.Get(t.Context(), key(n, "flow-log"), flow); err != nil {
		t.Fatal(err)
	}
	flow.SetAnnotations(map[string]string{"changed-after-review": "true"})
	if err := f.kube.Update(t.Context(), flow); err != nil {
		t.Fatal(err)
	}
	update.ValidateOnly, update.PlanId = false, preview.Plan.PlanId
	if _, err := f.svc.UpdateNetwork(t.Context(), update); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale apply: %v", err)
	}
	update.ValidateOnly, update.PlanId = true, ""
	fresh, err := f.svc.UpdateNetwork(t.Context(), update)
	if err != nil {
		t.Fatal(err)
	}
	update.ValidateOnly, update.PlanId = false, fresh.Plan.PlanId
	applied, err := f.svc.UpdateNetwork(t.Context(), update)
	if err != nil || !applied.GetUpdateRecorded() {
		t.Fatalf("fresh apply: %v %v", applied, err)
	}
}

type runtimeApply = runtime.ApplyConfiguration
