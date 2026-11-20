package config_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/config"
)

// platformConfigPath is where both Deployments mount the shared file.
const platformConfigPath = "/etc/cluster-vending/platform.yaml"

// nodeGroupVariables are the environment variables of the node group
// settings, which must not be set in a manifest: they win over the file.
func nodeGroupVariables() []string {
	_, envNames := known(config.ServeSettings(), config.ControllerSettings())
	var out []string
	for _, name := range envNames {
		if strings.HasPrefix(name, "CV_SYSTEM_NODE_GROUP_") || strings.HasPrefix(name, "CV_NODE_GROUP_") {
			out = append(out, name)
		}
	}
	return out
}

// No base sets a nodeGroups variable, as an env entry or a generator literal.
func TestNoBaseSetsNodeGroupVariables(t *testing.T) {
	banned := nodeGroupVariables()
	if len(banned) == 0 {
		t.Fatal("found no node group variables to check")
	}
	for _, name := range banned {
		for dir, vars := range map[string]map[string]string{"server/base": literals(t, serverBase)} {
			if _, set := vars[name]; set {
				t.Errorf("%s sets %s: it would override the shared file", dir, name)
			}
		}
		for _, e := range deployment(t, controllerDeployment).Spec.Template.Spec.Containers[0].Env {
			if e.Name == name {
				t.Errorf("%s sets %s: it would override the shared file", controllerDeployment, name)
			}
		}
		for _, e := range deployment(t, filepath.Join(serverBase, "deployment.yaml")).Spec.Template.Spec.Containers[0].Env {
			if e.Name == name {
				t.Errorf("server deployment sets %s", name)
			}
		}
	}
}

// built renders a kustomization with kubectl; the test is skipped without it.
func built(t *testing.T, dir string) []corev1.ConfigMap {
	t.Helper()
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl is not installed")
	}
	out, err := exec.Command(kubectl, "kustomize", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("kustomize %s: %v\n%s", dir, err, out)
	}
	var maps []corev1.ConfigMap
	for _, doc := range strings.Split(string(out), "\n---\n") {
		cm := corev1.ConfigMap{}
		if yaml.Unmarshal([]byte(doc), &cm) == nil && cm.Kind == "ConfigMap" {
			maps = append(maps, cm)
		}
	}
	return maps
}

// platformMap is the generated platform-config in a build, with its
// hashed name.
func platformMap(t *testing.T, dir string) corev1.ConfigMap {
	t.Helper()
	for _, cm := range built(t, dir) {
		if strings.Contains(cm.Name, "platform-config-") {
			return cm
		}
	}
	t.Fatalf("%s builds no platform-config ConfigMap", dir)
	return corev1.ConfigMap{}
}

// Both deliveries generate the one file with a content hash, and the
// Deployments mount that ConfigMap and pass its path to --config.
func TestPlatformConfigIsGeneratedAndMountedByBoth(t *testing.T) {
	want, err := os.ReadFile("node-groups/platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	server, controller := platformMap(t, serverBase), platformMap(t, "default")
	if server.Name != controller.Name {
		t.Errorf("the hash differs: %s and %s", server.Name, controller.Name)
	}
	for who, cm := range map[string]corev1.ConfigMap{"server": server, "controller": controller} {
		if cm.Data["platform.yaml"] != string(want) {
			t.Errorf("%s: platform.yaml is not node-groups/platform.yaml", who)
		}
	}
	for _, tc := range []struct {
		who string
		d   string
	}{{"controller", controllerDeployment}, {"server", filepath.Join(serverBase, "deployment.yaml")}} {
		spec := deployment(t, tc.d).Spec.Template.Spec
		if !slices.Contains(spec.Containers[0].Args, "--config="+platformConfigPath) {
			t.Errorf("%s does not pass --config=%s", tc.who, platformConfigPath)
		}
		mounted := false
		for _, m := range spec.Containers[0].VolumeMounts {
			mounted = mounted || m.MountPath == filepath.Dir(platformConfigPath)
		}
		if !mounted {
			t.Errorf("%s does not mount %s", tc.who, filepath.Dir(platformConfigPath))
		}
	}
}

// A change of the file changes the ConfigMap name, which rolls both
// Deployments (the name is in each pod template).
func TestPlatformConfigNameFollowsContent(t *testing.T) {
	cm := platformMap(t, serverBase)
	for _, dir := range []string{serverBase, "default"} {
		found := false
		for _, doc := range strings.Split(string(mustBuild(t, dir)), "\n---\n") {
			if strings.Contains(doc, "kind: Deployment") && strings.Contains(doc, "name: "+cm.Name) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no Deployment references %s", dir, cm.Name)
		}
	}
}

func mustBuild(t *testing.T, dir string) []byte {
	t.Helper()
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl is not installed")
	}
	out, err := exec.Command(kubectl, "kustomize", dir).Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The shared file loads for both commands, with the same layout.
func TestSharedPlatformFileLoadsForBothCommands(t *testing.T) {
	file, err := config.LoadFile("node-groups/platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	serve, err := config.LoadServe(nil, lookup(nil), file)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := config.LoadController(nil, lookup(map[string]string{config.EnvAWSManagementRoleARN: "arn:aws:iam::111122223333:role/r"}), file)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serve.NodeGroups, controller.NodeGroups) || !reflect.DeepEqual(serve.NodeGroups, config.DefaultNodeGroups()) {
		t.Errorf("serve %+v controller %+v", serve.NodeGroups, controller.NodeGroups)
	}
}

// The auth overlay's file is the shared file plus the auth section: same
// nodeGroups, so the server and the controller cannot disagree.
func TestAuthOverlayKeepsTheSharedNodeGroups(t *testing.T) {
	section := func(path string) any {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		return doc["nodeGroups"]
	}
	if a, b := section("node-groups/platform.yaml"), section("server/auth/platform.yaml"); !reflect.DeepEqual(a, b) {
		t.Errorf("server/auth/platform.yaml nodeGroups %v differs from node-groups/platform.yaml %v", b, a)
	}
	if _, err := config.LoadAuth("server/auth/platform.yaml"); err != nil {
		t.Error(err)
	}
	if _, err := config.LoadFile("server/auth/platform.yaml"); err != nil {
		t.Error(err)
	}
}
