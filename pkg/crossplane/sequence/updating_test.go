package sequence_test

import (
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// An object that is ready and moving to a new version (a node group EKS
// upgrades) meets "ready": what waits for it is created meanwhile, not
// held back for the length of the upgrade.
func TestAnUpdatingObjectIsReady(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&awseks.NodeGroup{}).Build()
	e, inv, ctx := sequence.Engine{Objects: managed.Client{Kube: k}}, &memory{scheme: scheme}, t.Context()
	nodes := &awseks.NodeGroup{ObjectMeta: meta("default")}
	nodes.Spec.ForProvider.Version = ptr.To("1.37")
	if _, err := e.Apply(ctx, sequence.Of(nodes), inv); err != nil {
		t.Fatal(err)
	}
	live := &awseks.NodeGroup{}
	if err := k.Get(ctx, client.ObjectKeyFromObject(nodes), live); err != nil {
		t.Fatal(err)
	}
	live.Status.AtProvider.Version = ptr.To("1.36")
	live.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
	if err := k.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	dependent := &awseks.Addon{ObjectMeta: meta("coredns")}

	res, err := e.Apply(ctx, sequence.Set{{Object: nodes}, {Object: dependent, After: []sequence.Condition{sequence.Ready(nodes)}}}, inv)

	if err != nil || !res.Complete() || res.Applied != 2 {
		t.Fatalf("apply = %+v (%v), want the add-on created while the node group updates", res, err)
	}
}
