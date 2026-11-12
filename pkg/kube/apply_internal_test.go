package kube

import (
	"strings"
	"testing"
	"time"

	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// lateMapper serves a kind only from the n-th lookup on, like an API
// server whose CRD is installed a moment later.
type lateMapper struct {
	meta.RESTMapper
	late    schema.GroupVersionKind
	lookups int
	from    int
}

func (m *lateMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if gk == m.late.GroupKind() {
		m.lookups++
		if m.lookups < m.from {
			return nil, &meta.NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
		}
	}
	return m.RESTMapper.RESTMapping(gk, versions...)
}

// The kinds of a provider installed at startup appear a little later:
// WaitForKinds waits for them, and names the ones that never come.
func TestWaitForKinds(t *testing.T) {
	kindPoll = time.Millisecond
	t.Cleanup(func() { kindPoll = 5 * time.Second })
	vpc := schema.GroupVersionKind{Group: "ec2.aws.m.upbound.io", Version: "v1beta1", Kind: "VPC"}
	group := schema.GroupVersionKind{Group: "cloudwatchlogs.aws.m.upbound.io", Version: "v1beta1", Kind: "Group"}
	key := schema.GroupVersionKind{Group: "kms.aws.m.upbound.io", Version: "v1beta1", Kind: "Key"}
	served := meta.NewDefaultRESTMapper(nil)
	served.Add(vpc, meta.RESTScopeNamespace)
	served.Add(group, meta.RESTScopeNamespace)

	mapper := &lateMapper{RESTMapper: served, late: group, from: 3}
	if err := WaitForKinds(t.Context(), mapper, []schema.GroupVersionKind{vpc, group}, time.Second); err != nil {
		t.Fatalf("WaitForKinds: %v", err)
	}
	if mapper.lookups < 3 {
		t.Errorf("%d lookups of the late kind, want it polled until served", mapper.lookups)
	}
	err := WaitForKinds(t.Context(), served, []schema.GroupVersionKind{vpc, key}, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "Key.kms.aws.m.upbound.io") || strings.Contains(err.Error(), "VPC") {
		t.Errorf("missing kind: %v, want an error naming only the KMS key", err)
	}
}
