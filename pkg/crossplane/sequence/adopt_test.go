package sequence_test

import (
	"context"
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// writes records, for one object, the external name each write to the
// management cluster carried: what the object's provider sees first.
type writes struct{ names []string }

func (w *writes) funcs(name string) interceptor.Funcs {
	note := func(obj client.Object) {
		if obj.GetName() == name {
			w.names = append(w.names, obj.GetAnnotations()[managed.ExternalNameAnnotation])
		}
	}
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			note(obj)
			return c.Create(ctx, obj, opts...)
		},
	}
}

func adoptWorld(t *testing.T, name string) (sequence.Engine, *memory, client.Client, *writes) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	w := &writes{}
	k := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(w.funcs(name)).Build()
	return sequence.Engine{Objects: managed.Client{Kube: k}}, &memory{scheme: scheme}, k, w
}

// An item that names the cloud resource it stands for (ExternalName) is
// created with that name in its very first write: its provider never sees
// it without, and so finds the existing resource instead of making a
// second one. A later apply that does not name it leaves the name alone.
func TestApplyCreatesWithTheExternalName(t *testing.T) {
	e, inv, k, w := adoptWorld(t, "vpc")
	ctx := t.Context()
	vpc := func() *awsec2.VPC { return &awsec2.VPC{ObjectMeta: meta("vpc")} }

	if _, err := e.Apply(ctx, sequence.Set{{Object: vpc(), ExternalName: "vpc-0abc"}}, inv); err != nil {
		t.Fatal(err)
	}

	if len(w.names) == 0 || w.names[0] != "vpc-0abc" {
		t.Fatalf("the object's first write carried external name %q, want vpc-0abc (writes: %v)", first(w.names), w.names)
	}
	live := vpc()
	if !exists(t, k, live) || live.Annotations[managed.ExternalNameAnnotation] != "vpc-0abc" {
		t.Fatalf("live external name = %q", live.Annotations[managed.ExternalNameAnnotation])
	}
	// The usual apply, which does not name the resource.
	if _, err := e.Apply(ctx, sequence.Of(vpc()), inv); err != nil {
		t.Fatal(err)
	}
	live = vpc()
	if !exists(t, k, live) || live.Annotations[managed.ExternalNameAnnotation] != "vpc-0abc" {
		t.Errorf("after a later apply the external name is %q: the object lost its cloud resource", live.Annotations[managed.ExternalNameAnnotation])
	}
}

// The name is for an object that is not there. One that exists keeps the
// name it has, and an object that brings its own name is applied as it
// is.
func TestExternalNameIsOnlyForAMissingObject(t *testing.T) {
	tests := []struct {
		name     string
		existing string // external name of the object already there; "" for none there
		rendered string // external name the item's object carries
		recorded string // Item.ExternalName
		want     string
	}{
		{"missing, recorded", "", "", "vpc-recorded", "vpc-recorded"},
		{"exists with another name", "vpc-live", "", "vpc-recorded", "vpc-live"},
		{"the render names it", "", "vpc-rendered", "vpc-recorded", "vpc-rendered"},
		{"nothing recorded", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, inv, k, _ := adoptWorld(t, "vpc")
			ctx := t.Context()
			if tt.existing != "" {
				there := &awsec2.VPC{ObjectMeta: meta("vpc")}
				there.Annotations = map[string]string{managed.ExternalNameAnnotation: tt.existing}
				if err := k.Create(ctx, there); err != nil {
					t.Fatal(err)
				}
			}
			obj := &awsec2.VPC{ObjectMeta: meta("vpc")}
			if tt.rendered != "" {
				obj.Annotations = map[string]string{managed.ExternalNameAnnotation: tt.rendered}
			}

			if _, err := e.Apply(ctx, sequence.Set{{Object: obj, ExternalName: tt.recorded}}, inv); err != nil {
				t.Fatal(err)
			}

			live := &awsec2.VPC{ObjectMeta: meta("vpc")}
			if !exists(t, k, live) || live.Annotations[managed.ExternalNameAnnotation] != tt.want {
				t.Errorf("external name = %q, want %q", live.Annotations[managed.ExternalNameAnnotation], tt.want)
			}
		})
	}
}

func first(s []string) string {
	if len(s) == 0 {
		return "(no write)"
	}
	return s[0]
}
