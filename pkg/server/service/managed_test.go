package service

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func TestObserveMissingObjects(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme"}}
	d := Deps{Kube: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()}
	views, err := d.Observe(t.Context(), []model.ManagedResource{
		{APIVersion: "v1", Kind: "Namespace", Name: "tenant-acme"},
		{APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "VPC", Namespace: "tenant-acme", Name: "gone"},
		{APIVersion: "example.io/v1", Kind: "Unknown", Namespace: "tenant-acme", Name: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []cloudv1.ManagedResourceState{
		cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY,
		cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING,
		cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING,
	}
	for i, w := range want {
		if views[i].GetState() != w {
			t.Errorf("view %d (%s) = %s, want %s", i, views[i].GetKind(), views[i].GetState(), w)
		}
	}
}
