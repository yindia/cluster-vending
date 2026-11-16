package naas

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"text/template"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func mustLoad(t *testing.T) *Bootstrap {
	t.Helper()
	b, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func documents(t *testing.T, objects []*unstructured.Unstructured) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, object := range objects {
		raw, err := json.Marshal(object.Object)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "---\n%s\n", raw)
	}
	return out.Bytes()
}

func TestRevisionTracksValuesAndIgnoresYAMLPresentation(t *testing.T) {
	b := mustLoad(t)
	reencoded, err := load(documents(t, b.Objects()))
	if err != nil {
		t.Fatal(err)
	}
	if b.Revision() != reencoded.Revision() || len(b.Revision()) != len("sha256:")+64 {
		t.Fatalf("noncanonical revision: %q vs %q", b.Revision(), reencoded.Revision())
	}
	commented, err := load(append([]byte("# presentation only\n\n"), bootstrapYAML...))
	if err != nil || commented.Revision() != b.Revision() {
		t.Fatalf("comment changed revision: %v", err)
	}
	objects := b.Objects()
	resources := objects[1].Object["spec"].(map[string]any)["resources"].([]any)
	generator := resources[0].(map[string]any)["generators"].([]any)[0].(map[string]any)
	generator["template"] = strings.Replace(generator["template"].(string), "restricted-ambient-v1", "restricted-ambient-v2", 1)
	changed, err := load(documents(t, objects))
	if err != nil {
		t.Fatal(err)
	}
	if changed.Revision() == b.Revision() {
		t.Fatal("values-only generator change did not change revision")
	}
	if !reflect.DeepEqual(mustLoad(t).Objects(), b.Objects()) {
		t.Fatal("snapshot mutation changed the canonical asset")
	}
}

func TestObjectsAndHelmValuesAreIndependentCopies(t *testing.T) {
	b := mustLoad(t)
	want := b.Objects()
	objects := b.Objects()
	objects[0].SetName("changed")
	objects[1].Object["spec"].(map[string]any)["resources"].([]any)[0].(map[string]any)["policy"].(map[string]any)["deletion"] = "Remove"
	values := b.HelmExtraManifests()
	values[1].(map[string]any)["spec"].(map[string]any)["resources"].([]any)[0].(map[string]any)["generators"].([]any)[0].(map[string]any)["template"] = "changed"
	if !reflect.DeepEqual(b.Objects(), want) {
		t.Fatal("caller modified the immutable snapshot through a returned object")
	}
	if reflect.DeepEqual(values, b.HelmExtraManifests()) {
		t.Fatal("HelmExtraManifests shares nested values across calls")
	}
}

func TestLoadRejectsInvalidOrAmbiguousPayload(t *testing.T) {
	for _, raw := range []string{"# empty", "not: [valid", "kind: Namespace\nmetadata: {name: x}", "[]"} {
		if _, err := load([]byte(raw)); err == nil {
			t.Errorf("accepted invalid payload %q", raw)
		}
	}
	object := "apiVersion: v1\nkind: Namespace\nmetadata: {name: duplicate}\n"
	if _, err := load([]byte(object + "---\n" + object)); err == nil {
		t.Fatal("accepted duplicate resource identities")
	}
}

// Execute the pinned chart's Go-template pipeline, including toYaml followed by
// tpl. The optional Helm CLI test also checks Helm's own YAML serialization.
func TestHelmTplRoundTripAndCapsuleGenerators(t *testing.T) {
	b := mustLoad(t)
	chartTemplate := "{{ range .Values.extraManifests }}\n---\n{{ tpl (toYaml .) $ }}\n{{ end }}\n"
	if raw, err := os.ReadFile(filepath.Join(localChartDir(), "templates", "extra-manifests.yaml")); err == nil {
		chartTemplate = string(raw)
	}
	funcs := template.FuncMap{
		"toYaml": func(value any) (string, error) {
			raw, err := yaml.Marshal(value)
			return string(raw), err
		},
		"tpl": func(source string, data any) (string, error) {
			var out bytes.Buffer
			tpl, err := template.New("helm-tpl").Option("missingkey=error").Parse(source)
			if err != nil {
				return "", err
			}
			err = tpl.Execute(&out, data)
			return out.String(), err
		},
	}
	tpl, err := template.New("extra-manifests").Funcs(funcs).Parse(chartTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, map[string]any{"Values": map[string]any{"extraManifests": b.HelmExtraManifests()}}); err != nil {
		t.Fatal(err)
	}
	roundTrip, err := load(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip.Objects(), b.Objects()) || roundTrip.Revision() != b.Revision() {
		t.Fatal("Helm tpl changed Capsule expressions or bootstrap content")
	}
	checkGenerators(t, roundTrip)
}

func checkGenerators(t *testing.T, b *Bootstrap) {
	t.Helper()
	const tenantName = "st-00000000000040008000000000000021"
	const group = `oidc:team:"quoted"\ops' {{ not-a-template }}`
	context := map[string]any{"tenant": map[string]any{
		"metadata": map[string]any{"name": tenantName, "uid": "tenant-uid", "labels": map[string]any{
			"cloud.cluster-vending.io/tenant": "acme", "cloud.cluster-vending.io/subtenant-id": "subtenant-id",
			"cloud.cluster-vending.io/access-ready": "false",
		}},
		"spec": map[string]any{"data": map[string]any{"naas": map[string]any{
			"namespaceKeys": []string{"app", "worker"}, "developerGroup": group, "pssVersion": "v1.35",
		}}},
	}}
	for _, object := range b.Objects() {
		if object.GetKind() != "GlobalTenantResource" {
			continue
		}
		scope, _, _ := unstructured.NestedString(object.Object, "spec", "scope")
		contexts := []map[string]any{context}
		if scope == "Namespace" {
			contexts = nil
			for _, key := range []string{"app", "worker"} {
				contexts = append(contexts, map[string]any{"tenant": context["tenant"], "namespace": map[string]any{"metadata": map[string]any{"name": tenantName + "-" + key}}})
			}
		}
		for _, ctx := range contexts {
			var rendered bytes.Buffer
			resources, _, _ := unstructured.NestedSlice(object.Object, "spec", "resources")
			for _, resource := range resources {
				for _, generator := range resource.(map[string]any)["generators"].([]any) {
					g := generator.(map[string]any)
					tpl, err := template.New(object.GetName()).Option("missingkey=" + g["missingKey"].(string)).Parse(g["template"].(string))
					if err != nil {
						t.Fatal(err)
					}
					rendered.WriteString("\n---\n")
					if err := tpl.Execute(&rendered, ctx); err != nil {
						t.Fatal(err)
					}
				}
			}
			generated, err := load(rendered.Bytes())
			if err != nil {
				t.Fatalf("%s: %v\n%s", object.GetName(), err, rendered.String())
			}
			switch object.GetName() {
			case "naas-namespace-bootstrap":
				if len(generated.objects) != 2 || generated.objects[0].GetName() != tenantName+"-app" || generated.objects[1].GetName() != tenantName+"-worker" {
					t.Fatal("namespace generator did not render both namespace keys")
				}
				for _, ns := range generated.objects {
					if ns.GetLabels()["cloud.cluster-vending.io/naas-ready"] != "false" || ns.GetOwnerReferences()[0].UID != "tenant-uid" {
						t.Fatal("namespace generator changed the readiness input or owner UID")
					}
				}
			case "naas-isolation-baseline":
				if len(generated.objects) != 8 {
					t.Fatalf("generated %d isolation resources, want 8", len(generated.objects))
				}
				for _, item := range generated.objects {
					if item.GetNamespace() != ctx["namespace"].(map[string]any)["metadata"].(map[string]any)["name"] {
						t.Fatalf("%s rendered into the wrong namespace", item.GetName())
					}
					if item.GetKind() == "AuthorizationPolicy" {
						rules, _, _ := unstructured.NestedSlice(item.Object, "spec", "rules")
						namespaces := rules[0].(map[string]any)["from"].([]any)[0].(map[string]any)["source"].(map[string]any)["namespaces"]
						if !reflect.DeepEqual(namespaces, []any{tenantName + "-app", tenantName + "-worker"}) {
							t.Fatalf("wrong authorized namespace inventory: %v", namespaces)
						}
					}
				}
			case "naas-developer-access":
				if len(generated.objects) != 1 {
					t.Fatal("unexpected access resources")
				}
				subjects, _, _ := unstructured.NestedSlice(generated.objects[0].Object, "subjects")
				if subjects[0].(map[string]any)["name"] != group {
					t.Fatal("quoted identity parameter was changed or interpreted as template source")
				}
			}
		}
	}
}

func TestBootstrapAuthorityAndRetention(t *testing.T) {
	b := mustLoad(t)
	if len(b.objects) != 4 {
		t.Fatalf("got %d bootstrap objects, want one ClusterRole and three GTRs", len(b.objects))
	}
	for _, object := range b.Objects() {
		if object.GetKind() == "ClusterRole" {
			if object.GetName() != "naas-workload-editor" {
				t.Fatal("unexpected bootstrap authority")
			}
			encoded, _ := json.Marshal(object.Object["rules"])
			for _, forbidden := range []string{`"*"`, `"bind"`, `"escalate"`, `"impersonate"`, `"secrets"`, `"rolebindings"`, `"networkpolicies"`} {
				if strings.Contains(string(encoded), forbidden) {
					t.Errorf("workload role contains %s", forbidden)
				}
			}
			continue
		}
		if object.GetKind() != "GlobalTenantResource" {
			t.Fatalf("unexpected per-tenant or authority object: %s", object.GetKind())
		}
		deletion := "Orphan"
		if object.GetName() == "naas-developer-access" {
			deletion = "Remove"
			gate, _, _ := unstructured.NestedString(object.Object, "spec", "tenantSelector", "matchLabels", "cloud.cluster-vending.io/access-ready")
			if gate != "true" {
				t.Fatal("developer access replication lost its explicit selector gate")
			}
		}
		resources, _, _ := unstructured.NestedSlice(object.Object, "spec", "resources")
		for _, resource := range resources {
			if resource.(map[string]any)["policy"].(map[string]any)["deletion"] != deletion {
				t.Fatalf("%s has the wrong retention policy", object.GetName())
			}
		}
	}
}
