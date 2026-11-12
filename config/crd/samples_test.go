package crd_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// componentSamples are the component sample files and the kind of each.
var componentSamples = map[string]string{
	"cloud_v1alpha1_component.yaml":           "Component",
	"cloud_v1alpha1_componentdefinition.yaml": "ComponentDefinition",
}

// sampleDocuments are the YAML documents of a samples file.
func sampleDocuments(t *testing.T, name string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "samples", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(raw, []byte("\n---\n"))
}

// The component samples decode strictly into the Go types: no field the
// kind does not have.
func TestComponentSamplesDecodeStrictly(t *testing.T) {
	for name, kind := range componentSamples {
		for i, doc := range sampleDocuments(t, name) {
			var obj runtime.Object = &cloudv1alpha1.Component{}
			if kind == "ComponentDefinition" {
				obj = &cloudv1alpha1.ComponentDefinition{}
			}
			if err := yaml.UnmarshalStrict(doc, obj); err != nil {
				t.Errorf("%s document %d: %v", name, i, err)
			}
			if got := obj.GetObjectKind().GroupVersionKind().Kind; got != kind {
				t.Errorf("%s document %d: kind %s, want %s", name, i, got, kind)
			}
		}
	}
}

// validator checks an object against a CRD's schema and CEL rules, as the
// API server does on create with strict field validation, without one: a
// field the schema does not have is an error too.
type validator struct {
	schema validation.SchemaValidator
	cel    *cel.Validator
	s      *structuralschema.Structural
}

func newValidator(t *testing.T, kind string) validator {
	t.Helper()
	def := embedded(t)[kind]
	crdv1 := &apiextensionsv1.CustomResourceDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(def.Object, crdv1); err != nil {
		t.Fatal(err)
	}
	internal := &apiextensionsinternal.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crdv1.Spec.Versions[0].Schema.OpenAPIV3Schema, internal, nil); err != nil {
		t.Fatal(err)
	}
	schema, _, err := validation.NewSchemaValidator(internal)
	if err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(internal)
	if err != nil {
		t.Fatal(err)
	}
	return validator{schema: schema, s: structural, cel: cel.NewValidator(structural, true, celconfig.PerCallLimit)}
}

func (v validator) validate(t *testing.T, doc []byte) field.ErrorList {
	t.Helper()
	obj := map[string]any{}
	if err := yaml.Unmarshal(doc, &obj); err != nil {
		t.Fatal(err)
	}
	var errs field.ErrorList
	for _, path := range pruning.PruneWithOptions(obj, v.s, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true}) {
		errs = append(errs, field.Forbidden(field.NewPath(path), "unknown field"))
	}
	errs = append(errs, validation.ValidateCustomResource(nil, obj, v.schema)...)
	celErrs, _ := v.cel.Validate(t.Context(), nil, v.s, obj, nil, celconfig.RuntimeCELCostBudget)
	return append(errs, celErrs...)
}

// Every component sample is one the API server accepts: the CRD's schema
// and its CEL rules hold.
func TestComponentSamplesPassTheCRDs(t *testing.T) {
	for name, kind := range componentSamples {
		v := newValidator(t, kind)
		for i, doc := range sampleDocuments(t, name) {
			if errs := v.validate(t, doc); len(errs) > 0 {
				t.Errorf("%s document %d: %v", name, i, errs.ToAggregate())
			}
		}
	}
}

// The rules refuse what they are for.
func TestComponentDefinitionRules(t *testing.T) {
	v := newValidator(t, "ComponentDefinition")
	base := `apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ComponentDefinition
metadata:
  name: %s
spec:
  description: d
  namespace: tools
  install:
`
	for name, tt := range map[string]struct {
		name, install string
		wantErr       bool
	}{
		"public https":    {"tools", "    helm: {repository: 'https://charts.example.com', chart: tools}", false},
		"private basic":   {"tools", "    helm: {repository: 'oci://registry.example.com/charts', chart: tools, credentials: {type: BasicAuth, secretRef: {namespace: cluster-vending-system, name: charts}}}", false},
		"plain http oci":  {"tools", "    helm: {repository: 'oci://registry.local:5000/charts', chart: tools, plainHTTP: true, insecureSkipTLSVerify: true}", false},
		"no helm":         {"tools", "    {}", true},
		"plain http repo": {"tools", "    helm: {repository: 'https://charts.example.com', chart: tools, plainHTTP: true}", true},
		"ftp repository":  {"tools", "    helm: {repository: 'ftp://charts.example.com', chart: tools}", true},
		"secret without namespace": {"tools",
			"    helm: {repository: 'oci://r.example.com/c', chart: tools, credentials: {type: BasicAuth, secretRef: {name: charts}}}", true},
		"inline password": {"tools",
			"    helm: {repository: 'oci://r.example.com/c', chart: tools, credentials: {type: BasicAuth, password: hunter2, secretRef: {namespace: ns, name: charts}}}", true},
		"unknown credential type": {"tools",
			"    helm: {repository: 'oci://r.example.com/c', chart: tools, credentials: {type: Token, secretRef: {namespace: ns, name: charts}}}", true},
		"not a DNS label": {"Tools", "    helm: {repository: 'https://charts.example.com', chart: tools}", true},
	} {
		doc := []byte(fmtDoc(base, tt.name) + tt.install + "\n")
		errs := v.validate(t, doc)
		if tt.wantErr && len(errs) == 0 {
			t.Errorf("%s: accepted", name)
		}
		if !tt.wantErr && len(errs) > 0 {
			t.Errorf("%s: %v", name, errs.ToAggregate())
		}
	}
}

func fmtDoc(base, name string) string {
	return string(bytes.Replace([]byte(base), []byte("%s"), []byte(name), 1))
}

// A Component's identity is fixed, and its name is <cluster>-<component>.
func TestComponentRules(t *testing.T) {
	v := newValidator(t, "Component")
	doc := func(name, component string) []byte {
		return []byte(`apiVersion: cloud.cluster-vending.io/v1alpha1
kind: Component
metadata:
  name: ` + name + `
  namespace: tenant-acme
spec:
  clusterID: 01a1d0a6-69d5-7e15-965e-fc6a53fa3d33
  component: ` + component + `
  values: {replicas: 2}
`)
	}
	if errs := v.validate(t, doc("prod-eks-karpenter", "karpenter")); len(errs) > 0 {
		t.Errorf("valid component: %v", errs.ToAggregate())
	}
	if errs := v.validate(t, doc("prod-eks", "karpenter")); len(errs) == 0 {
		t.Error("a component not named <cluster>-<component> was accepted")
	}
	if errs := v.validate(t, doc("prod-eks-Karpenter", "Karpenter")); len(errs) == 0 {
		t.Error("a component name that is not a DNS label was accepted")
	}
	old := map[string]any{}
	if err := yaml.Unmarshal(doc("prod-eks-karpenter", "karpenter"), &old); err != nil {
		t.Fatal(err)
	}
	moved := map[string]any{}
	if err := yaml.Unmarshal(bytes.Replace(doc("prod-eks-karpenter", "karpenter"),
		[]byte("01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"), []byte("02b2e1b7-7ae6-7f26-a76f-0d7b6eab4e44"), 1), &moved); err != nil {
		t.Fatal(err)
	}
	if errs, _ := v.cel.Validate(t.Context(), nil, v.s, moved, old, celconfig.RuntimeCELCostBudget); len(errs) == 0 {
		t.Error("a component moved to another cluster")
	}
}
