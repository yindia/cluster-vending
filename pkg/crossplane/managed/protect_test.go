package managed_test

import (
	"context"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/kube"
)

var noDelete = xpv2.ManagementPolicies{xpv2.ManagementActionObserve, xpv2.ManagementActionCreate,
	xpv2.ManagementActionUpdate, xpv2.ManagementActionLateInitialize}

// policiesAtDelete is a client that records each object's management
// policies when it is deleted.
func policiesAtDelete(t *testing.T, objects ...client.Object) (managed.Client, map[string]xpv2.ManagementPolicies) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]xpv2.ManagementPolicies{}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			v := &awsec2.VPC{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), v); err == nil {
				seen[obj.GetName()] = v.Spec.ManagementPolicies
			}
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	return managed.Client{Kube: k}, seen
}

func protectedVPC(name string, annotations map[string]string) *awsec2.VPC {
	v := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: name, Annotations: annotations}}
	v.Spec.ManagementPolicies = noDelete
	return v
}

func ref(name string) managed.Ref {
	return managed.Ref{APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "VPC", Namespace: "tenant-acme", Name: name}
}

// The owner's deletion switches a protected object to full management
// before deleting it (its cloud resource goes), but not a shared one
// (RetainAnnotation); a render's drop does the same, except in a code
// change (KeepDropped): the cloud resource stays.
func TestDeleteReleasesProtectedObjects(t *testing.T) {
	c, seen := policiesAtDelete(t, protectedVPC("owned", nil), protectedVPC("shared", map[string]string{managed.RetainAnnotation: "true"}),
		protectedVPC("dropped", nil), protectedVPC("dropped-by-code", nil))
	ctx := t.Context()

	if err := c.Delete(ctx, []managed.Ref{ref("owned"), ref("shared")}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDropped(ctx, ref("dropped")); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDropped(managed.KeepDropped(ctx), ref("dropped-by-code")); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{"owned": "*", "shared": "no Delete", "dropped": "*", "dropped-by-code": "no Delete"}
	for name, w := range want {
		p := seen[name]
		got := "no Delete"
		if len(p) == 1 && p[0] == xpv2.ManagementActionAll {
			got = "*"
		}
		if got != w {
			t.Errorf("%s deleted with policies %v, want %s", name, p, w)
		}
	}
}
