package catalog_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	shipped "github.com/yindia/cluster-vending/config/catalog"
	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func newClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func shippedNames(t *testing.T) []string {
	t.Helper()
	defs, err := shipped.Definitions()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	return names
}

// Every shipped definition is created on an empty cluster; a second install
// creates nothing.
func TestInstallCreatesMissingOnce(t *testing.T) {
	c := newClient(t)

	first, err := catalog.Install(t.Context(), c)

	if err != nil {
		t.Fatal(err)
	}
	if want := shippedNames(t); !slices.Equal(first.Created, want) || len(first.Existing) != 0 {
		t.Fatalf("first install = %+v, want all of %v created", first, want)
	}
	second, err := catalog.Install(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Created) != 0 || len(second.Existing) != len(first.Created) {
		t.Errorf("second install = %+v, want nothing created", second)
	}
}

// A definition the platform team already has is left as it is.
func TestInstallKeepsAnEditedDefinition(t *testing.T) {
	edited := &cloudv1alpha1.ComponentDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "karpenter"},
		Spec: cloudv1alpha1.ComponentDefinitionSpec{Description: "ours", Namespace: "karpenter",
			Install: &cloudv1alpha1.ComponentInstall{Helm: &cloudv1alpha1.HelmInstall{
				HelmChartSource: cloudv1alpha1.HelmChartSource{Repository: "oci://example.com/charts", Chart: "karpenter"}}}},
	}
	c := newClient(t, edited)

	res, err := catalog.Install(t.Context(), c)

	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Existing, "karpenter") || slices.Contains(res.Created, "karpenter") {
		t.Errorf("result = %+v, want karpenter left as it was", res)
	}
	got := &cloudv1alpha1.ComponentDefinition{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "karpenter"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Description != "ours" || got.Spec.Namespace != "karpenter" {
		t.Errorf("karpenter was changed: %+v", got.Spec)
	}
}

// A failed create stops the install and names the definition.
func TestInstallReportsAFailedCreate(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			return errors.New("forbidden")
		},
	}).Build()

	_, err = catalog.Install(t.Context(), c)

	if err == nil || !strings.Contains(err.Error(), "create component definition") {
		t.Errorf("err = %v", err)
	}
}

// Create does not change the definitions it is given.
func TestCreateLeavesItsInputAlone(t *testing.T) {
	def := &cloudv1alpha1.ComponentDefinition{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
	if _, err := catalog.Create(t.Context(), newClient(t), []*cloudv1alpha1.ComponentDefinition{def}); err != nil {
		t.Fatal(err)
	}
	if def.ResourceVersion != "" {
		t.Errorf("the input got a resourceVersion: %q", def.ResourceVersion)
	}
}
