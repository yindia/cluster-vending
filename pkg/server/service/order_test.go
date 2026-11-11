package service

import (
	"context"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func TestDeleteNextGoesTierByTier(t *testing.T) {
	ctx := context.Background()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: "t", Name: name} }
	ng := &awseks.NodeGroup{ObjectMeta: meta("ng")}
	ng.Spec.ForProvider.Region = ptr.To("us-east-1")
	eks := &awseks.Cluster{ObjectMeta: meta("eks")}
	eks.Spec.ForProvider.Region = ptr.To("us-east-1")
	role := &awsiam.Role{ObjectMeta: meta("role")}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ng, eks, role).Build()
	d := Deps{Kube: k}

	// Recorded in creation order: role, cluster, node group.
	records := []model.ManagedResource{
		{APIVersion: "iam.aws.m.upbound.io/v1beta1", Kind: "Role", Namespace: "t", Name: "role"},
		{APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "Cluster", Namespace: "t", Name: "eks"},
		{APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "NodeGroup", Namespace: "t", Name: "ng"},
	}
	order := DeleteOrder{{"NodeGroup"}, {"Cluster"}, {"Role"}}
	exists := func(obj client.Object) bool {
		return k.Get(ctx, client.ObjectKeyFromObject(obj), obj) == nil
	}
	step := func(wantLeft int) {
		t.Helper()
		views, err := d.Observe(ctx, records)
		if err != nil {
			t.Fatal(err)
		}
		left, err := d.DeleteNext(ctx, records, views, order)
		if err != nil || left != wantLeft {
			t.Fatalf("left %d err %v, want %d", left, err, wantLeft)
		}
	}

	step(3) // deletes the node group only
	if exists(&awseks.NodeGroup{ObjectMeta: meta("ng")}) || !exists(&awseks.Cluster{ObjectMeta: meta("eks")}) ||
		!exists(&awsiam.Role{ObjectMeta: meta("role")}) {
		t.Fatal("first step must delete only the node group")
	}
	step(2) // node group gone: deletes the cluster
	if exists(&awseks.Cluster{ObjectMeta: meta("eks")}) || !exists(&awsiam.Role{ObjectMeta: meta("role")}) {
		t.Fatal("second step must delete only the cluster")
	}
	step(1) // deletes the role
	step(0)
	if !AllGone(must(d.Observe(ctx, records))) {
		t.Fatal("not all gone")
	}
	_ = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
