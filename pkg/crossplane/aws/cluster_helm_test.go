package aws_test

import (
	"encoding/json"
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// Every cluster has a provider-helm ProviderConfig reaching it with the
// kubeconfig its ClusterAuth writes, in the shape the
// providerconfigs.helm.m.crossplane.io v1beta1 CRD requires (credentials
// source and a secretRef with namespace, name and key), and a Usage keeps
// the ClusterAuth while it exists.
func TestHelmProviderConfig(t *testing.T) {
	plan := clusterPlan()
	var pc *unstructured.Unstructured
	var usage *protectionv1beta1.Usage
	for _, obj := range crossplaneaws.ClusterResources(plan) {
		switch o := obj.(type) {
		case *unstructured.Unstructured:
			if o.GroupVersionKind() == crossplaneaws.HelmProviderConfigGVK {
				pc = o
			}
		case *protectionv1beta1.Usage:
			if o.Spec.By != nil && o.Spec.By.Kind == "ProviderConfig" {
				usage = o
			}
		}
	}
	if pc == nil {
		t.Fatal("no helm ProviderConfig rendered")
	}
	got, err := json.Marshal(pc.Object)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"apiVersion":"helm.m.crossplane.io/v1beta1","kind":"ProviderConfig","metadata":{"labels":{"app.kubernetes.io/managed-by":"cluster-vending","cluster-vending.io/cluster":"c-1","cluster-vending.io/tenant-id":"acme"},"name":"prod-1a2b3c4d-helm","namespace":"tenant-acme"},"spec":{"credentials":{"secretRef":{"key":"kubeconfig","name":"prod-1a2b3c4d-kubeconfig","namespace":"tenant-acme"},"source":"Secret"}}}`
	if string(got) != want {
		t.Errorf("helm ProviderConfig\n got %s\nwant %s", got, want)
	}
	if pc.GetName() != crossplaneaws.HelmProviderConfigName(plan.Prefix) {
		t.Errorf("name %s", pc.GetName())
	}
	if usage == nil || usage.Spec.Of.Kind != "ClusterAuth" || usage.Spec.Of.ResourceRef.Name != plan.Prefix+"-auth" ||
		usage.Spec.By.APIVersion != "helm.m.crossplane.io/v1beta1" || usage.Spec.By.ResourceRef.Name != pc.GetName() {
		t.Errorf("Usage keeping the ClusterAuth for the ProviderConfig = %+v", usage)
	}
}

// The ProviderConfig is applied once the ClusterAuth is Ready (its
// kubeconfig Secret exists), and the Usage that protects the ClusterAuth
// for it waits with it.
func TestHelmProviderConfigWaitsForTheClusterAuth(t *testing.T) {
	for _, item := range crossplaneaws.ClusterSet(clusterPlan()) {
		isPC := false
		if u, ok := item.Object.(*unstructured.Unstructured); ok && u.GroupVersionKind() == crossplaneaws.HelmProviderConfigGVK {
			isPC = true
		}
		if u, ok := item.Object.(*protectionv1beta1.Usage); ok && u.Spec.By != nil && u.Spec.By.Kind == "ProviderConfig" {
			isPC = true
		}
		if isPC && len(item.After) != 1 {
			t.Errorf("%s waits for %d conditions, want the ClusterAuth Ready", item.Object.GetName(), len(item.After))
		}
	}
}
