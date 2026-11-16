package catalog_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/naas"
)

func TestCapsuleBootstrapPinPreservesPDBAndMinorValues(t *testing.T) {
	overrides := map[string]any{"replicaCount": float64(3), "extraManifests": []any{
		map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "minor-extra"}},
	}}
	before, _ := json.Marshal(overrides)
	values, err := catalog.CapsuleBootstrapValues(overrides)
	if err != nil {
		t.Fatal(err)
	}
	if values["replicaCount"] != float64(3) {
		t.Fatal("minor values were lost")
	}
	bootstrap, _ := naas.Load()
	extra := values["extraManifests"].([]any)
	if len(extra) != len(bootstrap.Objects())+2 || extra[0].(map[string]any)["kind"] != "PodDisruptionBudget" || extra[1].(map[string]any)["kind"] != "ConfigMap" {
		t.Fatal("Helm list replacement lost the PDB or minor extra manifests")
	}
	for _, item := range extra[2:] {
		annotations := item.(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)
		if annotations["cloud.cluster-vending.io/naas-bootstrap-revision"] != bootstrap.Revision() {
			t.Fatal("missing pinned revision")
		}
	}
	if after, _ := json.Marshal(overrides); !bytes.Equal(before, after) {
		t.Fatal("pin composition mutated the caller")
	}
	// A new call must not inherit mutations from another minor's pin.
	extra[2].(map[string]any)["metadata"].(map[string]any)["name"] = "changed"
	again, err := catalog.CapsuleBootstrapValues(overrides)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(values, again) {
		t.Fatal("pins share mutable bootstrap objects")
	}
	if _, err := catalog.CapsuleBootstrapValues(map[string]any{"extraManifests": "invalid"}); err == nil {
		t.Fatal("accepted invalid extraManifests")
	}
}

func TestLocalCapsuleBootstrapPinHelmRender(t *testing.T) {
	chart := os.Getenv("CAPSULE_CHART_DIR")
	if chart == "" {
		chart = "/tmp/capsule"
	}
	raw, err := os.ReadFile(filepath.Join(chart, "Chart.yaml"))
	if os.IsNotExist(err) && os.Getenv("CAPSULE_CHART_DIR") == "" {
		t.Skip("local pinned chart unavailable; no network used")
	}
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Version    string `json:"version"`
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Version != "0.15.2" || metadata.AppVersion != "0.15.2" {
		t.Fatal("local chart must be chart/app 0.15.2")
	}
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm unavailable; no installation attempted")
	}
	values, err := catalog.CapsuleBootstrapValues(nil)
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := component.Expand(values, component.Vars{"system.nodeSelector": `{}`, "system.tolerations": `[]`})
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(expanded)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), helm, "template", "platform-capsule", chart, "--namespace", "capsule-system", "--values", file)
	cmd.Env = append(os.Environ(), "HELM_CACHE_HOME="+filepath.Join(dir, "cache"), "HELM_CONFIG_HOME="+filepath.Join(dir, "config"), "HELM_DATA_HOME="+filepath.Join(dir, "data"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("offline pinned Helm render: %v\n%s", err, out)
	}
	decoder := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	seen := map[string]bool{}
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		seen[object.GetKind()+"/"+object.GetName()] = true
		if object.GetKind() == "ClusterRoleBinding" {
			role, _, _ := unstructured.NestedString(object.Object, "roleRef", "name")
			if role == "cluster-admin" {
				t.Fatal("rendered chart granted cluster-admin")
			}
		}
		if object.GetKind() == "ClusterRole" {
			rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
			for _, rule := range rules {
				for _, verb := range rule.(map[string]any)["verbs"].([]any) {
					if verb == "impersonate" {
						t.Fatal("chart retained cluster-wide impersonation")
					}
				}
			}
		}
		if object.GetKind() == "CapsuleConfiguration" {
			identity := []any{map[string]any{"kind": "ServiceAccount", "name": "system:serviceaccount:naas-system:provisioner"}}
			for _, field := range []string{"users", "administrators"} {
				actual, _, _ := unstructured.NestedSlice(object.Object, "spec", field)
				if !reflect.DeepEqual(actual, identity) {
					t.Fatalf("unexpected Capsule %s: %v", field, actual)
				}
			}
			roles, _, _ := unstructured.NestedSlice(object.Object, "spec", "rbac", "administrationClusterRoles")
			if len(roles) != 0 {
				t.Fatal("Capsule administrator mapping adds automatic role grants")
			}
		}
		if object.GetKind() == "GlobalTenantResource" {
			// The actual Helm tpl pipeline must retain Capsule expressions exactly.
			bootstrap, _ := naas.Load()
			for _, expected := range bootstrap.Objects() {
				if expected.GetKind() == object.GetKind() && expected.GetName() == object.GetName() && !reflect.DeepEqual(expected.Object["spec"], object.Object["spec"]) {
					t.Fatal("Helm interpreted or changed a Capsule generator")
				}
			}
		}
	}
	for _, required := range []string{"PodDisruptionBudget/capsule-platform", "Namespace/naas-system", "ServiceAccount/provisioner", "ClusterRole/naas-provisioner", "ClusterRole/naas-capsule-controller", "Role/naas-capsule-impersonate-provisioner", "CapsuleConfiguration/default", "GlobalTenantResource/naas-namespace-bootstrap", "GlobalTenantResource/naas-isolation-baseline", "GlobalTenantResource/naas-developer-access"} {
		if !seen[required] {
			t.Errorf("missing %s", required)
		}
	}
}
