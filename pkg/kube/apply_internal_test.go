package kube

import (
	"testing"

	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func TestApplyConfiguration(t *testing.T) {
	s, err := Scheme()
	if err != nil {
		t.Fatal(err)
	}
	// No TypeMeta: apiVersion and kind come from the scheme.
	pc := &awsv1beta1.ProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "aws", Namespace: "tenant-acme"},
		Spec: awsv1beta1.ProviderConfigSpec{
			Credentials:     awsv1beta1.ProviderCredentials{Source: "IRSA"},
			AssumeRoleChain: []awsv1beta1.AssumeRoleOptions{{RoleARN: ptr.To("arn:aws:iam::444455556666:role/x")}},
		},
	}
	u, err := applyConfiguration(pc, s)
	if err != nil {
		t.Fatal(err)
	}
	if u.GetAPIVersion() != "aws.m.upbound.io/v1beta1" || u.GetKind() != "ProviderConfig" {
		t.Errorf("type = %s %s", u.GetAPIVersion(), u.GetKind())
	}
	if _, ok := u.Object["status"]; ok {
		t.Error("status must not be applied")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "metadata", "creationTimestamp"); found {
		t.Error("creationTimestamp must not be applied")
	}
	if src, _, _ := unstructured.NestedString(u.Object, "spec", "credentials", "source"); src != "IRSA" {
		t.Errorf("credentials.source = %q", src)
	}
	if pc.Kind != "" {
		t.Error("the input object was modified")
	}

	if _, err := applyConfiguration(&corev1.Namespace{}, runtime.NewScheme()); err == nil {
		t.Error("want an error for a type the scheme does not know")
	}
}
