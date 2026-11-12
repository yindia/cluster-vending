package crd_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/config/crd/crdtest"
	"github.com/yindia/cluster-vending/pkg/component"
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
// API server does on create with strict field validation (crdtest).
type validator struct{ v *crdtest.Validator }

func newValidator(t *testing.T, kind string) validator {
	t.Helper()
	v, err := crdtest.New(kind)
	if err != nil {
		t.Fatal(err)
	}
	return validator{v: v}
}

func (v validator) validate(t *testing.T, doc []byte) field.ErrorList {
	t.Helper()
	errs, err := v.v.Validate(t.Context(), doc)
	if err != nil {
		t.Fatal(err)
	}
	return errs
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

// A definition has a chart of its own or is a bundle, never both; a chart
// needs a namespace; a bundle is not internal; management is Platform or
// Tenant; a CRD chart is a chart source.
func TestComponentDefinitionShapeRules(t *testing.T) {
	v := newValidator(t, "ComponentDefinition")
	const head = "apiVersion: cloud.cluster-vending.io/v1alpha1\nkind: ComponentDefinition\nmetadata:\n  name: x\nspec:\n  description: d\n"
	const install = "  install:\n    helm: {repository: 'https://charts.example.com', chart: tools}\n"
	for name, tt := range map[string]struct {
		spec    string
		wantErr bool
	}{
		"chart":                     {"  namespace: tools\n" + install, false},
		"platform chart":            {"  management: Platform\n  namespace: tools\n" + install, false},
		"internal member":           {"  internal: true\n  namespace: tools\n" + install, false},
		"bundle":                    {"  includes: [a, b]\n", false},
		"chart with a CRD chart":    {"  namespace: tools\n  install:\n    helm: {repository: 'https://c.example.com', chart: tools, crds: {repository: 'oci://r.example.com/c', chart: tools-crds}}\n", false},
		"version group and stepped": {"  namespace: tools\n  versionGroup: istio\n  upgradeOneMinorAtATime: true\n" + install, false},
		"neither chart nor bundle":  {"  namespace: tools\n", true},
		"chart and bundle":          {"  namespace: tools\n  includes: [a]\n" + install, true},
		"chart without a namespace": {install, true},
		"internal bundle":           {"  internal: true\n  includes: [a]\n", true},
		"empty bundle":              {"  includes: []\n", true},
		"unknown management":        {"  management: Operator\n  namespace: tools\n" + install, true},
		"CRD chart without a chart": {"  namespace: tools\n  install:\n    helm: {repository: 'https://c.example.com', chart: tools, crds: {repository: 'oci://r.example.com/c'}}\n", true},
		"version group not a label": {"  namespace: tools\n  versionGroup: Istio Mesh\n" + install, true},
	} {
		errs := v.validate(t, []byte(head+tt.spec))
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
	moved := bytes.Replace(doc("prod-eks-karpenter", "karpenter"),
		[]byte("01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"), []byte("02b2e1b7-7ae6-7f26-a76f-0d7b6eab4e44"), 1)
	errs, err := v.v.ValidateUpdate(t.Context(), moved, doc("prod-eks-karpenter", "karpenter"))
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) == 0 {
		t.Error("a component moved to another cluster")
	}
}

// catalogEntry is a sample definition as pkg/component reads it.
func catalogEntry(t *testing.T, def *cloudv1alpha1.ComponentDefinition) component.Definition {
	t.Helper()
	object := func(raw *runtime.RawExtension) component.Values {
		if raw == nil {
			return nil
		}
		var v component.Values
		if err := yaml.Unmarshal(raw.Raw, &v); err != nil {
			t.Fatalf("%s: %v", def.Name, err)
		}
		return v
	}
	return component.Definition{Name: def.Name, Valid: true, Requires: def.Spec.Requires,
		ConflictsWithAddons: def.Spec.ConflictsWithAddons, PlatformValues: object(def.Spec.PlatformValues),
		UserValues: def.Spec.UserValues, ValuesSchema: object(def.Spec.ValuesSchema)}
}

// sampleDefinitions are the sample definitions by name.
func sampleDefinitions(t *testing.T) map[string]component.Definition {
	t.Helper()
	out := map[string]component.Definition{}
	for _, doc := range sampleDocuments(t, "cloud_v1alpha1_componentdefinition.yaml") {
		def := &cloudv1alpha1.ComponentDefinition{}
		if err := yaml.UnmarshalStrict(doc, def); err != nil {
			t.Fatal(err)
		}
		out[def.Name] = catalogEntry(t, def)
	}
	return out
}

// Every sample definition is valid as pkg/component judges it: a
// supported values schema, and a userValues file a tenant can send back as
// it is (an object, no platformValues path, valid against the schema).
func TestSampleDefinitionsAreValid(t *testing.T) {
	files := 0
	for name, d := range sampleDefinitions(t) {
		if d.UserValues != "" {
			files++
		}
		if problems := component.CheckDefinition(d); len(problems) > 0 {
			t.Errorf("%s: %v", name, problems)
		}
	}
	if files == 0 {
		t.Error("no sample definition has a userValues file")
	}
}

// The sample Component is one EnableComponent accepts: its values are real
// edits of the userValues file, touch no platformValues path and validate
// against the values schema.
func TestSampleComponentValuesAreAccepted(t *testing.T) {
	definitions := sampleDefinitions(t)
	for _, doc := range sampleDocuments(t, "cloud_v1alpha1_component.yaml") {
		c := &cloudv1alpha1.Component{}
		if err := yaml.UnmarshalStrict(doc, c); err != nil {
			t.Fatal(err)
		}
		d, ok := definitions[c.Spec.Component]
		if !ok {
			t.Fatalf("%s: no sample definition %s", c.Name, c.Spec.Component)
		}
		var values component.Values
		if err := yaml.Unmarshal(c.Spec.Values.Raw, &values); err != nil {
			t.Fatal(err)
		}
		file, err := component.ParseUserValues(d.UserValues)
		if err != nil {
			t.Fatal(err)
		}
		if pruned := component.PruneUnchanged(values, file); !reflect.DeepEqual(pruned, values) {
			t.Errorf("%s stores %v, want only edits %v", c.Name, values, pruned)
		}
		if locked := component.LockedPathsTouched(values, d.PlatformValues); len(locked) > 0 {
			t.Errorf("%s sets locked paths %v", c.Name, locked)
		}
		if problems := component.ValidateSchema(d.ValuesSchema, values); len(problems) > 0 {
			t.Errorf("%s: %v", c.Name, problems)
		}
	}
}
