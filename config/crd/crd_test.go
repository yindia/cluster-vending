package crd_test

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/yindia/cluster-vending/config/crd"
)

// The controller installs what is embedded: every kind, each with its
// status subresource, and the contract status kept as it is written.
func TestEmbeddedDefinitions(t *testing.T) {
	definitions, err := crd.All()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]*unstructured.Unstructured{}
	for _, d := range definitions {
		kind, _, _ := unstructured.NestedString(d.Object, "spec", "names", "kind")
		kinds[kind] = d
	}
	want := []string{"Tenant", "SubTenant", "CloudAccount", "Network", "Cluster", "Release", "ReleaseChannel", "ReleaseHold", "Component", "ComponentDefinition"}
	for _, kind := range want {
		if kinds[kind] == nil {
			t.Errorf("no definition of %s", kind)
		}
	}
	if len(definitions) != len(want) {
		t.Fatalf("%d definitions of kinds %v, want %v", len(definitions), kinds, want)
	}
	// The operator's release objects are cluster-scoped, with a status
	// the controller writes.
	for _, kind := range []string{"Tenant", "Release", "ReleaseChannel", "ReleaseHold", "ComponentDefinition"} {
		if scope, _, _ := unstructured.NestedString(kinds[kind].Object, "spec", "scope"); scope != "Cluster" {
			t.Errorf("%s scope = %s, want Cluster", kind, scope)
		}
		versions, _, _ := unstructured.NestedSlice(kinds[kind].Object, "spec", "versions")
		if _, served, _ := unstructured.NestedMap(versions[0].(map[string]any), "subresources", "status"); !served {
			t.Errorf("%s has no status subresource", kind)
		}
	}
	for _, kind := range []string{"Network", "Cluster"} {
		versions, _, _ := unstructured.NestedSlice(kinds[kind].Object, "spec", "versions")
		if len(versions) != 1 {
			t.Fatalf("%s: %d versions", kind, len(versions))
		}
		version := versions[0].(map[string]any)
		if _, served, _ := unstructured.NestedMap(version, "subresources", "status"); !served {
			t.Errorf("%s has no status subresource", kind)
		}
		contract, found, _ := unstructured.NestedMap(version, "schema", "openAPIV3Schema", "properties", "status", "properties", "contractStatus")
		if !found || contract["x-kubernetes-preserve-unknown-fields"] != true {
			t.Errorf("%s status.contractStatus = %v, want a field that keeps what it is given", kind, contract)
		}
	}
}

// A Component is a tenant object: namespaced, in the tenant namespace, with
// a status the controller writes and values kept as the tenant wrote them.
func TestComponentKind(t *testing.T) {
	kinds := embedded(t)
	component := kinds["Component"]
	if scope, _, _ := unstructured.NestedString(component.Object, "spec", "scope"); scope != "Namespaced" {
		t.Errorf("Component scope = %s, want Namespaced", scope)
	}
	versions, _, _ := unstructured.NestedSlice(component.Object, "spec", "versions")
	version := versions[0].(map[string]any)
	if _, served, _ := unstructured.NestedMap(version, "subresources", "status"); !served {
		t.Error("Component has no status subresource")
	}
	for _, path := range [][]string{
		{"spec", "properties", "values"},
		{"status", "properties", "effectiveValues"},
	} {
		field, found, _ := unstructured.NestedMap(version, append([]string{"schema", "openAPIV3Schema", "properties"}, path...)...)
		if !found || field["x-kubernetes-preserve-unknown-fields"] != true || field["type"] != "object" {
			t.Errorf("Component %v = %v, want an object that keeps what it is given", path, field)
		}
	}
}

// conflictsWithAddons names EKS add-ons: exactly the names AWSAddon.name
// accepts, so a definition cannot name an add-on the platform does not know.
func TestConflictsWithAddonsMatchesAddons(t *testing.T) {
	kinds := embedded(t)
	enum := func(kind string, path ...string) []any {
		versions, _, _ := unstructured.NestedSlice(kinds[kind].Object, "spec", "versions")
		values, found, _ := unstructured.NestedSlice(versions[0].(map[string]any), append([]string{"schema", "openAPIV3Schema", "properties"}, path...)...)
		if !found || len(values) == 0 {
			t.Fatalf("%s %v: no enum", kind, path)
		}
		return values
	}
	addons := enum("Cluster", "spec", "properties", "aws", "properties", "addons", "items", "properties", "name", "enum")
	conflicts := enum("ComponentDefinition", "spec", "properties", "conflictsWithAddons", "items", "enum")
	if !reflect.DeepEqual(addons, conflicts) {
		t.Errorf("conflictsWithAddons accepts %v, want the add-ons %v", conflicts, addons)
	}
}

// A definition refers to the Secret of a private chart repository; it has
// no field that could hold a username, password or token, and the
// Secret's namespace is required. The main chart and each companion alike.
func TestChartCredentialsAreReferences(t *testing.T) {
	kinds := embedded(t)
	versions, _, _ := unstructured.NestedSlice(kinds["ComponentDefinition"].Object, "spec", "versions")
	helm := []string{"schema", "openAPIV3Schema", "properties", "spec", "properties", "install", "properties", "helm"}
	for name, source := range map[string][]string{
		"helm":       helm,
		"companions": append(slices.Clone(helm), "properties", "companions", "items"),
	} {
		credentials, found, _ := unstructured.NestedMap(versions[0].(map[string]any), append(source, "properties", "credentials")...)
		if !found {
			t.Fatalf("%s: no credentials", name)
		}
		fields, _, _ := unstructured.NestedMap(credentials, "properties")
		if got := slices.Sorted(maps.Keys(fields)); !slices.Equal(got, []string{"secretRef", "type"}) {
			t.Errorf("%s.credentials fields = %v, want only secretRef and type", name, got)
		}
		required, _, _ := unstructured.NestedStringSlice(credentials, "properties", "secretRef", "required")
		if !slices.Contains(required, "namespace") || !slices.Contains(required, "name") {
			t.Errorf("%s.credentials.secretRef requires %v, want namespace and name", name, required)
		}
		if credentials["x-kubernetes-preserve-unknown-fields"] == true {
			t.Errorf("%s.credentials keeps unknown fields", name)
		}
	}
}

func embedded(t *testing.T) map[string]*unstructured.Unstructured {
	t.Helper()
	definitions, err := crd.All()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]*unstructured.Unstructured{}
	for _, d := range definitions {
		kind, _, _ := unstructured.NestedString(d.Object, "spec", "names", "kind")
		kinds[kind] = d
	}
	return kinds
}

// Every CEL rule is plain ASCII. gofmt rewrites two single quotes in a doc
// comment, where the markers live, into a typographic quote, which CEL
// cannot compile and only a real API server reports: rules test for an
// empty string with size(x) == 0 instead.
func TestValidationRulesAreASCII(t *testing.T) {
	defs, err := crd.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range defs {
		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, child := range x {
					if k == "rule" {
						if s, ok := child.(string); ok {
							for i, r := range s {
								if r > 0x7e {
									t.Errorf("%s: %s.rule has %q at %d: %s", def.GetName(), path, r, i, s)
									break
								}
							}
						}
						continue
					}
					walk(path+"."+k, child)
				}
			case []any:
				for _, child := range x {
					walk(path+"[]", child)
				}
			}
		}
		walk("", def.Object)
	}
}
