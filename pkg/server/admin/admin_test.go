package admin

import (
	"testing"

	"connectrpc.com/connect/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

const (
	clusterA = "0190a000-0000-7000-8000-00000000000a"
	clusterB = "0190a000-0000-7000-8000-00000000000b"
)

func TestPauseAndResumeReconcile(t *testing.T) {
	ctx := t.Context()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	ng := func(ns, name string, labels map[string]string) *awseks.NodeGroup {
		o := &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
		o.Spec.ForProvider.Region = ptr.To("us-east-1")
		return o
	}
	vpc := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "net-vpc", Labels: crossplane.NetworkLabels("acme", "n-1")}}
	vpc.Spec.ForProvider.Region = ptr.To("us-east-1")
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		ng("tenant-acme", "a-default", crossplane.ClusterLabels("acme", clusterA)),
		ng("tenant-acme", "b-default", crossplane.ClusterLabels("acme", clusterB)),
		ng("tenant-beta", "c-default", crossplane.ClusterLabels("beta", clusterA)), vpc).Build()
	s := NewService(reconcile.Deps{Kube: k})
	paused := func(obj client.Object) bool {
		t.Helper()
		if err := k.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		return obj.GetAnnotations()[PausedAnnotation] == "true"
	}
	a, b := ng("tenant-acme", "a-default", nil), ng("tenant-acme", "b-default", nil)
	other := ng("tenant-beta", "c-default", nil)

	// One cluster.
	res, err := s.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{TenantId: "acme", ClusterId: clusterA})
	if err != nil || res.GetResources() != 1 || !paused(a) || paused(b) || paused(other) || paused(vpc) {
		t.Fatalf("pause one cluster: %v, %d resources; a %v b %v other tenant %v vpc %v", err, res.GetResources(),
			paused(a), paused(b), paused(other), paused(vpc))
	}
	// The whole tenant, networks included; again is a no-op with the same count.
	for range 2 {
		res, err = s.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{TenantId: "acme"})
		if err != nil || res.GetResources() != 3 || !paused(a) || !paused(b) || !paused(vpc) || paused(other) {
			t.Fatalf("pause the tenant: %v, %d resources", err, res.GetResources())
		}
	}
	resumed, err := s.ResumeReconcile(ctx, &cloudv1.ResumeReconcileRequest{TenantId: "acme", ClusterId: clusterB})
	if err != nil || resumed.GetResources() != 1 || paused(b) || !paused(a) {
		t.Fatalf("resume one cluster: %v, %d resources; a %v b %v", err, resumed.GetResources(), paused(a), paused(b))
	}
	if _, err := s.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{TenantId: "Not_Valid"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad tenant: %v, want INVALID_ARGUMENT", err)
	}
}
