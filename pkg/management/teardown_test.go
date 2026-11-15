package management_test

import (
	"slices"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/management"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func karpenter(group, kind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: "v1", Kind: kind})
	u.SetName(name)
	return u
}

func teardownScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func refsOf(objs []reconcile.WorkloadObject) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Ref())
	}
	sort.Strings(out)
	return out
}

// The teardown sees the Ingresses the AWS Load Balancer Controller serves
// (its IngressClasses, the alb class, the old annotation), every Service
// of type LoadBalancer, and Karpenter's objects; nothing else.
func TestTeardownList(t *testing.T) {
	scheme := teardownScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "karpenter.sh", Version: "v1", Kind: "NodePool"},
		{Group: "karpenter.sh", Version: "v1", Kind: "NodeClaim"},
		{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass"},
	} {
		mapper.Add(gvk, meta.RESTScopeRoot)
	}
	for _, gvk := range []schema.GroupVersionKind{
		corev1.SchemeGroupVersion.WithKind("Service"), networkingv1.SchemeGroupVersion.WithKind("Ingress"),
	} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	mapper.Add(networkingv1.SchemeGroupVersion.WithKind("IngressClass"), meta.RESTScopeRoot)
	ingress := func(name string, class *string, annotation string) *networkingv1.Ingress {
		in := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: name}, Spec: networkingv1.IngressSpec{IngressClassName: class}}
		if annotation != "" {
			in.Annotations = map[string]string{"kubernetes.io/ingress.class": annotation}
		}
		return in
	}
	objs := []client.Object{
		&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "public"}, Spec: networkingv1.IngressClassSpec{Controller: "ingress.k8s.aws/alb"}},
		ingress("by-class", ptr.To("public"), ""),
		ingress("default-alb", ptr.To("alb"), ""),
		ingress("annotated", nil, "alb"),
		ingress("nginx", ptr.To("nginx"), ""),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "gw"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "internal"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}},
		karpenter("karpenter.sh", "NodePool", "default"),
		karpenter("karpenter.sh", "NodeClaim", "default-x2k"),
		karpenter("karpenter.k8s.aws", "EC2NodeClass", "default"),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objs...).Build()
	td := management.TeardownOf(c)

	got, err := td.List(t.Context())

	if err != nil {
		t.Fatal(err)
	}
	want := []string{"EC2NodeClass default", "Ingress web/annotated", "Ingress web/by-class", "Ingress web/default-alb",
		"NodeClaim default-x2k", "NodePool default", "Service istio-system/gw"}
	if g := refsOf(got); !slices.Equal(g, want) {
		t.Fatalf("listed %v, want %v", g, want)
	}

	for _, o := range got {
		if err := td.Delete(t.Context(), o); err != nil {
			t.Fatalf("delete %s: %v", o.Ref(), err)
		}
	}
	if left, err := td.List(t.Context()); err != nil || len(left) != 0 {
		t.Fatalf("after delete: %v, %v", refsOf(left), err)
	}
	if err := td.Delete(t.Context(), reconcile.WorkloadObject{Kind: reconcile.KindService, Namespace: "web", Name: "gone"}); err != nil {
		t.Errorf("deleting what is gone: %v", err)
	}
}

// A cluster without Karpenter is no error: its kinds are left out.
func TestTeardownWithoutKarpenter(t *testing.T) {
	scheme := teardownScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "gw"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}},
	).Build()

	got, err := management.TeardownOf(c).List(t.Context())

	if err != nil || len(got) != 1 || got[0].Ref() != "Service istio-system/gw" {
		t.Fatalf("listed %v, %v", refsOf(got), err)
	}
}
