package sequence_test

import (
	"context"
	"slices"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/kube"
)

const ns = "tenant-acme"

// memory is an inventory in memory.
type memory struct {
	scheme  *runtime.Scheme
	entries []sequence.Entry
}

func (m *memory) List(context.Context) ([]sequence.Entry, error) { return slices.Clone(m.entries), nil }

func (m *memory) Add(_ context.Context, objects []client.Object) error {
	for _, obj := range objects {
		ref, err := managed.RefOf(obj, m.scheme)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(m.entries, func(e sequence.Entry) bool { return e.Ref == ref }) {
			m.entries = append(m.entries, sequence.Entry{Ref: ref})
		}
	}
	return nil
}

func (m *memory) Remove(_ context.Context, entry sequence.Entry) error {
	m.entries = slices.DeleteFunc(m.entries, func(e sequence.Entry) bool { return e.Ref == entry.Ref })
	return nil
}

func (m *memory) names() []string {
	out := make([]string, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e.Kind+"/"+e.Name)
	}
	return out
}

func world(t *testing.T) (sequence.Engine, *memory, client.Client) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&awsec2.InternetGateway{}).Build()
	return sequence.Engine{Objects: managed.Client{Kube: k}}, &memory{scheme: scheme}, k
}

func meta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }

func exists(t *testing.T, k client.Client, obj client.Object) bool {
	t.Helper()
	err := k.Get(t.Context(), client.ObjectKeyFromObject(obj), obj)
	if err != nil && client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	return err == nil
}

// An item is held back until what it waits for is Ready, nothing is
// removed meanwhile, and once the owner has the object it is applied
// whatever its conditions say.
func TestApplyHoldsBackWhatWaits(t *testing.T) {
	e, inv, k := world(t)
	ctx := t.Context()
	gateway := &awsec2.InternetGateway{ObjectMeta: meta("igw")}
	nat := &awsec2.NATGateway{ObjectMeta: meta("nat")}
	old := &awsec2.EIP{ObjectMeta: meta("dropped")}
	if _, err := e.Apply(ctx, sequence.Of(old), inv); err != nil {
		t.Fatal(err)
	}
	set := sequence.Set{{Object: gateway}, {Object: nat, After: []sequence.Condition{sequence.Ready(gateway)}}}

	res, err := e.Apply(ctx, set, inv)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete() || res.Applied != 1 || len(res.Waiting) != 1 || res.Waiting[0].Ref.Name != "nat" ||
		res.Waiting[0].For[0] != "InternetGateway igw to be ready" {
		t.Fatalf("first apply = %+v, want the gateway applied and the NAT gateway waiting for it", res)
	}
	if exists(t, k, &awsec2.NATGateway{ObjectMeta: meta("nat")}) {
		t.Error("the NAT gateway was created before the internet gateway was ready")
	}
	if !exists(t, k, &awsec2.EIP{ObjectMeta: meta("dropped")}) {
		t.Error("an object was removed while the set was not applied in full")
	}

	live := &awsec2.InternetGateway{}
	if err := k.Get(ctx, client.ObjectKeyFromObject(gateway), live); err != nil {
		t.Fatal(err)
	}
	live.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
	if err := k.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	res, err = e.Apply(ctx, set, inv)
	if err != nil || !res.Complete() || res.Applied != 2 || len(res.Removed) != 1 || res.Removed[0].Name != "dropped" {
		t.Fatalf("second apply = %+v (%v), want both applied and the dropped object removed", res, err)
	}
	if exists(t, k, &awsec2.EIP{ObjectMeta: meta("dropped")}) || slices.Contains(inv.names(), "EIP/dropped") {
		t.Errorf("the dropped object is still there: %v", inv.names())
	}

	// The gateway is not Ready any more (an update): the NAT gateway the
	// owner has is still applied.
	if err := k.Get(ctx, client.ObjectKeyFromObject(gateway), live); err != nil {
		t.Fatal(err)
	}
	live.Status.Conditions = nil
	if err := k.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	if res, err = e.Apply(ctx, set, inv); err != nil || !res.Complete() || res.Applied != 2 {
		t.Errorf("apply with the gateway not ready = %+v (%v), want the NAT gateway applied again", res, err)
	}
}

// What an inventory marks as kept stays when the set does not have it.
func TestApplyKeepsWhatIsMarkedKept(t *testing.T) {
	e, inv, k := world(t)
	ctx := t.Context()
	carried := &corev1.ConfigMap{ObjectMeta: meta("carried-over")}
	if _, err := e.Apply(ctx, sequence.Of(carried), inv); err != nil {
		t.Fatal(err)
	}
	inv.entries[0].Kept = true
	res, err := e.Apply(ctx, sequence.Of(&corev1.ConfigMap{ObjectMeta: meta("rendered")}), inv)
	if err != nil || len(res.Removed) != 0 {
		t.Fatalf("apply = %+v (%v), want nothing removed", res, err)
	}
	if !exists(t, k, &corev1.ConfigMap{ObjectMeta: meta("carried-over")}) {
		t.Error("a kept object was removed")
	}
}

// Objects are deleted tier by tier: the next tier only once the previous
// one is gone.
func TestDeleteGoesTierByTier(t *testing.T) {
	e, inv, k := world(t)
	ctx := t.Context()
	vpc, subnet := &awsec2.VPC{ObjectMeta: meta("vpc")}, &awsec2.Subnet{ObjectMeta: meta("subnet")}
	if _, err := e.Apply(ctx, sequence.Of(vpc, subnet), inv); err != nil {
		t.Fatal(err)
	}
	order := sequence.DeleteOrder{{"Subnet"}, {"VPC"}}
	for step, want := range []struct {
		left        int
		vpc, subnet bool
	}{{2, true, false}, {1, false, false}, {0, false, false}} {
		d, err := e.Delete(ctx, inv, order)
		if err != nil {
			t.Fatal(err)
		}
		if d.Left != want.left || d.Done() != (want.left == 0) || len(d.Views) != 2 {
			t.Errorf("step %d: %d left, done %t, %d views; want %d left", step, d.Left, d.Done(), len(d.Views), want.left)
		}
		if got := exists(t, k, &awsec2.VPC{ObjectMeta: meta("vpc")}); got != want.vpc {
			t.Errorf("step %d: VPC exists = %t", step, got)
		}
		if got := exists(t, k, &awsec2.Subnet{ObjectMeta: meta("subnet")}); got != want.subnet {
			t.Errorf("step %d: subnet exists = %t", step, got)
		}
	}
}
