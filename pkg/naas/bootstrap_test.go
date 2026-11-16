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
	"k8s.io/apimachinery/pkg/runtime"
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
			"namespaceKeys": []any{"app", "worker"}, "developerGroup": group, "pssVersion": "v1.35",
			"accessBindings": []any{map[string]any{"id": "admin-binding", "group": group, "role": WorkloadAdminRole}, map[string]any{"id": "viewer-binding", "group": "oidc:Observers", "role": WorkloadViewerRole}},
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
				contexts = append(contexts, map[string]any{"tenant": context["tenant"], "namespace": map[string]any{"metadata": map[string]any{"name": tenantName + "-" + key,
					"ownerReferences": []any{map[string]any{"apiVersion": "capsule.clastix.io/v1beta2", "kind": "Tenant", "name": tenantName, "uid": "tenant-uid"}}}}})
			}
		}
		for _, ctx := range contexts {
			if object.GetName() == "naas-developer-access" {
				ctx = runtime.DeepCopyJSON(ctx)
				ctx["tenant"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)[LabelAccessReady] = "true"
			}
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
				if len(generated.objects) != 2 {
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
	if len(b.objects) != 16 {
		t.Fatalf("got %d bootstrap objects, want three GTRs, three workload roles and ten trusted prerequisites", len(b.objects))
	}
	for _, object := range b.Objects() {
		if object.GetKind() == "ClusterRole" && object.GetName() == "naas-workload-editor" {
			encoded, _ := json.Marshal(object.Object["rules"])
			for _, forbidden := range []string{`"*"`, `"bind"`, `"escalate"`, `"impersonate"`, `"secrets"`, `"rolebindings"`, `"networkpolicies"`} {
				if strings.Contains(string(encoded), forbidden) {
					t.Errorf("workload role contains %s", forbidden)
				}
			}
			continue
		}
		if object.GetKind() != "GlobalTenantResource" {
			continue // Trusted bootstrap authority is checked separately below.
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

// Resolve the actual embedded RBAC bindings, including namespace scope. This
// catches additive grants that a check of a single restricted Role would miss.
func bootstrapAllows(t *testing.T, b *Bootstrap, subjectNS, subject, namespace, group, resource, name, verb string) bool {
	t.Helper()
	objects := b.Objects()
	for _, binding := range objects {
		kind := binding.GetKind()
		if kind != "RoleBinding" && kind != "ClusterRoleBinding" {
			continue
		}
		if kind == "RoleBinding" && binding.GetNamespace() != namespace {
			continue
		}
		subjects, _, _ := unstructured.NestedSlice(binding.Object, "subjects")
		matched := false
		for _, item := range subjects {
			s := item.(map[string]any)
			if s["kind"] == "ServiceAccount" && s["name"] == subject && s["namespace"] == subjectNS {
				matched = true
			}
		}
		if !matched {
			continue
		}
		ref := binding.Object["roleRef"].(map[string]any)
		for _, role := range objects {
			if role.GetKind() != ref["kind"] || role.GetName() != ref["name"] {
				continue
			}
			if role.GetKind() == "Role" && role.GetNamespace() != binding.GetNamespace() {
				continue
			}
			rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
			for _, item := range rules {
				r := item.(map[string]any)
				matches := func(field, value string) bool {
					values, present := r[field].([]any)
					if field == "resourceNames" && !present {
						return true
					}
					for _, entry := range values {
						if entry == value || (entry == "*" && field != "resourceNames") {
							return true
						}
					}
					return false
				}
				if matches("apiGroups", group) && matches("resources", resource) && matches("resourceNames", name) && matches("verbs", verb) {
					return true
				}
			}
		}
	}
	return false
}

func TestBootstrapReplicationAuthorityIsConstrained(t *testing.T) {
	b := mustLoad(t)
	for _, test := range []struct {
		subjectNS, subject, namespace, group, resource, name, verb string
		want                                                       bool
	}{
		{"capsule-system", "capsule", "naas-system", "", "serviceaccounts", "provisioner", "impersonate", true},
		{"capsule-system", "capsule", "other-system", "", "serviceaccounts", "provisioner", "impersonate", false},
		{"capsule-system", "capsule", "naas-system", "", "serviceaccounts", "other", "impersonate", false},
		{"capsule-system", "capsule", "", "", "users", "system:admin", "impersonate", false},
		{"capsule-system", "capsule", "", "", "groups", "system:masters", "impersonate", false},
		{"capsule-system", "capsule", "", "rbac.authorization.k8s.io", "clusterroles", "naas-workload-editor", "bind", true},
		{"capsule-system", "capsule", "", "rbac.authorization.k8s.io", "clusterroles", "admin", "bind", false},
		{"capsule-system", "capsule", "", "rbac.authorization.k8s.io", "clusterroles", "cluster-admin", "bind", false},
		{"naas-system", "provisioner", "", "", "namespaces", "st-example-app", "create", true},
		{"naas-system", "provisioner", "", "", "namespaces", "st-example-app", "delete", false},
		{"naas-system", "provisioner", "st-example-app", "networking.k8s.io", "networkpolicies", "naas-deny-by-default", "create", true},
		{"naas-system", "provisioner", "st-example-app", "security.istio.io", "authorizationpolicies", "naas-allow-owned-namespaces", "create", true},
		{"naas-system", "provisioner", "st-example-app", "", "limitranges", "naas-container-defaults", "create", true},
		{"naas-system", "provisioner", "st-example-app", "", "serviceaccounts", "app", "create", true},
		{"naas-system", "provisioner", "st-example-app", "rbac.authorization.k8s.io", "rolebindings", "naas-developers", "create", true},
		{"naas-system", "provisioner", "", "rbac.authorization.k8s.io", "clusterroles", "naas-workload-editor", "bind", true},
		{"naas-system", "provisioner", "", "rbac.authorization.k8s.io", "clusterroles", "admin", "bind", false},
		{"naas-system", "provisioner", "", "rbac.authorization.k8s.io", "clusterroles", "cluster-admin", "bind", false},
		{"naas-system", "provisioner", "", "rbac.authorization.k8s.io", "clusterroles", "naas-workload-editor", "escalate", false},
		{"naas-system", "provisioner", "st-example-app", "", "secrets", "credentials", "get", false},
		{"naas-system", "provisioner", "naas-system", "", "serviceaccounts/token", "provisioner", "create", false},
		{"naas-system", "provisioner", "", "", "nodes", "node", "patch", false},
	} {
		if got := bootstrapAllows(t, b, test.subjectNS, test.subject, test.namespace, test.group, test.resource, test.name, test.verb); got != test.want {
			t.Errorf("%s/%s %s %s/%s in %q: allowed=%t want %t", test.subjectNS, test.subject, test.verb, test.resource, test.name, test.namespace, got, test.want)
		}
	}
	for _, object := range b.Objects() {
		if object.GetKind() == "ServiceAccount" {
			value, found, _ := unstructured.NestedBool(object.Object, "automountServiceAccountToken")
			if !found || value {
				t.Fatal("bootstrap provisioner must not mount a credential")
			}
		}
		if object.GetKind() != "RoleBinding" && object.GetKind() != "ClusterRoleBinding" {
			continue
		}
		ref := object.Object["roleRef"].(map[string]any)
		if ref["name"] == "cluster-admin" {
			t.Fatal("bootstrap binds cluster-admin")
		}
		subjects, _, _ := unstructured.NestedSlice(object.Object, "subjects")
		for _, subject := range subjects {
			if subject.(map[string]any)["kind"] != "ServiceAccount" {
				t.Fatal("bootstrap grants human access")
			}
		}
	}
}
