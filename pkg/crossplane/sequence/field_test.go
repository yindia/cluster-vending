package sequence_test

import (
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

// eksCluster is an EKS cluster reporting status ("" for none yet).
func eksCluster(status string) *awseks.Cluster {
	c := &awseks.Cluster{ObjectMeta: meta("prod")}
	if status != "" {
		c.Status.AtProvider.Status = ptr.To(status)
	}
	return c
}

// An item waiting for a field is created once the object has the value.
// Once the owner has an object that waited for it, the condition was
// passed: that object and new ones waiting for the same are applied
// whatever the field says.
func TestFieldCondition(t *testing.T) {
	active := sequence.Field(&awseks.Cluster{ObjectMeta: meta("prod")}, "ACTIVE", "status", "atProvider", "status")
	for _, tt := range []struct {
		name string
		// eks is the EKS cluster in the management cluster; nil for none.
		eks *awseks.Cluster
		// has: the owner already has the add-on.
		has  bool
		want bool
	}{
		{"no EKS cluster yet", nil, false, false},
		{"EKS cluster without status", eksCluster(""), false, false},
		{"EKS creating", eksCluster("CREATING"), false, false},
		{"EKS active", eksCluster("ACTIVE"), false, true},
		// An upgrade makes EKS UPDATING: add-ons created before stay
		// applied, and one added now is created.
		{"add-on created, EKS updating", eksCluster("UPDATING"), true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, inv, k := world(t)
			ctx := t.Context()
			if tt.eks != nil {
				if err := k.Create(ctx, tt.eks); err != nil {
					t.Fatal(err)
				}
			}
			addon := func() *awseks.Addon { return &awseks.Addon{ObjectMeta: meta("prod-addon-coredns")} }
			if tt.has {
				if _, err := e.Apply(ctx, sequence.Of(addon()), inv); err != nil {
					t.Fatal(err)
				}
			}
			set := sequence.Set{
				{Object: addon(), After: []sequence.Condition{active}},
				{Object: &awseks.Addon{ObjectMeta: meta("prod-addon-vpc-cni")}, After: []sequence.Condition{active}},
			}
			res, err := e.Apply(ctx, set, inv)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"prod-addon-coredns", "prod-addon-vpc-cni"} {
				if got := exists(t, k, &awseks.Addon{ObjectMeta: meta(name)}); got != tt.want {
					t.Errorf("add-on %s applied = %t, want %t (%+v)", name, got, tt.want, res)
				}
			}
			if !tt.want && (len(res.Waiting) != 2 || res.Waiting[0].For[0] != "Cluster prod to have status.atProvider.status ACTIVE") {
				t.Errorf("waiting = %+v, want both add-ons waiting for the EKS cluster to be ACTIVE", res.Waiting)
			}
		})
	}
}

// An item's Adjust changes what is sent, not the item's object.
func TestApplyAdjustsTheBody(t *testing.T) {
	e, inv, k := world(t)
	ctx := t.Context()
	item := sequence.Item{
		Object: &corev1.ConfigMap{ObjectMeta: meta("cm"), Data: map[string]string{"sent": "no", "kept": "yes"}},
		Adjust: func(body *unstructured.Unstructured) { unstructured.RemoveNestedField(body.Object, "data", "sent") },
	}
	if res, err := e.Apply(ctx, sequence.Set{item}, inv); err != nil || res.Applied != 1 {
		t.Fatalf("apply = %+v (%v)", res, err)
	}
	live := &corev1.ConfigMap{}
	if err := k.Get(ctx, client.ObjectKeyFromObject(item.Object), live); err != nil {
		t.Fatal(err)
	}
	if _, sent := live.Data["sent"]; sent || live.Data["kept"] != "yes" {
		t.Errorf("applied data = %v, want the adjusted body", live.Data)
	}
	if item.Object.(*corev1.ConfigMap).Data["sent"] != "no" {
		t.Error("Adjust changed the item's object")
	}
}

// Keeping keeps what it selects, on top of what the inventory keeps.
func TestKeeping(t *testing.T) {
	e, inv, k := world(t)
	ctx := t.Context()
	retiring, dropped := &corev1.ConfigMap{ObjectMeta: meta("retiring")}, &corev1.ConfigMap{ObjectMeta: meta("dropped")}
	if _, err := e.Apply(ctx, sequence.Of(retiring, dropped), inv); err != nil {
		t.Fatal(err)
	}
	keep := sequence.Keeping(inv, func(entry sequence.Entry) bool { return entry.Name == "retiring" })
	res, err := e.Apply(ctx, sequence.Of(&corev1.ConfigMap{ObjectMeta: meta("rendered")}), keep)
	if err != nil || len(res.Removed) != 1 || res.Removed[0].Name != "dropped" {
		t.Fatalf("apply = %+v (%v), want only the dropped object removed", res, err)
	}
	if !exists(t, k, &corev1.ConfigMap{ObjectMeta: meta("retiring")}) || exists(t, k, &corev1.ConfigMap{ObjectMeta: meta("dropped")}) {
		t.Error("the kept object was removed, or the dropped one was not")
	}
}
