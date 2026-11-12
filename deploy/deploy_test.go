// Package deploy_test keeps the deploy manifests (deploy/server and
// controller/config) in step with the code they run: the settings they
// pass are settings the commands accept, and the RBAC covers the kinds the
// platform renders. It reads the manifests as files; no kustomize, Docker
// or cluster is needed.
package deploy_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

const (
	controllerDeployment = "../controller/config/manager/manager.yaml"
	controllerRole       = "../controller/config/rbac/platform_role.yaml"
	serverBase           = "server/base"
	serverOverlays       = "server/overlays"
)

// documents are the YAML documents of a manifest file.
func documents(t *testing.T, path string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(raw, []byte("\n---\n"))
}

// deployment is the Deployment in a manifest file.
func deployment(t *testing.T, path string) *appsv1.Deployment {
	t.Helper()
	for _, doc := range documents(t, path) {
		d := &appsv1.Deployment{}
		if err := yaml.Unmarshal(doc, d); err == nil && d.Kind == "Deployment" {
			return d
		}
	}
	t.Fatalf("%s has no Deployment", path)
	return nil
}

// clusterRole is the ClusterRole in a manifest file.
func clusterRole(t *testing.T, path string) *rbacv1.ClusterRole {
	t.Helper()
	role := &rbacv1.ClusterRole{}
	if err := yaml.UnmarshalStrict(documents(t, path)[0], role); err != nil || role.Kind != "ClusterRole" {
		t.Fatalf("%s: not a ClusterRole (%v)", path, err)
	}
	return role
}

// literals are the configMapGenerator literals of a kustomization, as
// environment variables.
func literals(t *testing.T, dir string) map[string]string {
	t.Helper()
	var k struct {
		ConfigMapGenerator []struct {
			Literals []string `json:"literals"`
		} `json:"configMapGenerator"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, g := range k.ConfigMapGenerator {
		for _, l := range g.Literals {
			name, value, ok := strings.Cut(l, "=")
			if !ok {
				t.Fatalf("%s: literal %q has no value", dir, l)
			}
			out[name] = value
		}
	}
	return out
}

// lookup is a config.LookupFunc over a map.
func lookup(values map[string]string) config.LookupFunc {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

// flags are a container's "--name=value" arguments by flag name.
func flags(t *testing.T, args []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, arg := range args {
		name, value, ok := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !strings.HasPrefix(arg, "--") || !ok {
			t.Fatalf("argument %q is not --name=value", arg)
		}
		out[name] = value
	}
	return out
}

// known are the flag and environment variable names of settings.
func known(settings ...[]config.Setting) (flagNames, envNames []string) {
	for _, s := range slices.Concat(settings...) {
		flagNames, envNames = append(flagNames, s.Flag), append(envNames, s.Env)
	}
	return flagNames, envNames
}

// The controller's Deployment passes only settings the controller command
// has, and they load: the manager would start with them.
func TestControllerDeploymentUsesTheCommandsSettings(t *testing.T) {
	c := deployment(t, controllerDeployment).Spec.Template.Spec.Containers[0]
	if !slices.Equal(c.Command, []string{"/cluster-vending", "controller"}) {
		t.Errorf("command = %v", c.Command)
	}
	flagNames, envNames := known(config.ControllerSettings(), config.GlobalSettings())
	args := flags(t, c.Args)
	for name := range args {
		if !slices.Contains(flagNames, name) {
			t.Errorf("--%s is not a flag of the controller command", name)
		}
	}
	env := map[string]string{}
	for _, e := range c.Env {
		if !slices.Contains(envNames, e.Name) {
			t.Errorf("%s is not an environment variable of the controller command", e.Name)
		}
		env[e.Name] = "cluster-vending-system" // the only one is the namespace, from the downward API
	}
	cfg, err := config.LoadController(lookup(args), lookup(env), nil)
	if err != nil {
		t.Fatalf("the Deployment's settings do not load: %v", err)
	}
	if !cfg.LeaderElect || cfg.LeaseNamespace != "cluster-vending-system" || cfg.ProbeAddr != ":8081" || cfg.MetricsAddr != "0" {
		t.Errorf("loaded %+v", cfg)
	}
	// The metrics component replaces this argument by position.
	if c.Args[2] != "--metrics-addr=0" {
		t.Errorf("args[2] = %s; controller/config/metrics patches --metrics-addr there", c.Args[2])
	}
}

// Each server overlay's settings are settings the serve command has, and
// they load as the backend the overlay is for.
func TestServerOverlaysLoad(t *testing.T) {
	_, envNames := known(config.ServeSettings(), config.GlobalSettings())
	c := deployment(t, filepath.Join(serverBase, "deployment.yaml")).Spec.Template.Spec.Containers[0]
	if !slices.Equal(c.Command, []string{"/cluster-vending", "serve"}) || len(c.Args) != 0 {
		t.Errorf("command %v args %v", c.Command, c.Args)
	}
	for overlay, store := range map[string]string{"kubernetes-store": config.StoreKubernetes, "postgres": config.StoreDatabase} {
		env := literals(t, serverBase)
		for name, value := range literals(t, filepath.Join(serverOverlays, overlay)) {
			env[name] = value
		}
		for _, e := range c.Env {
			env[e.Name] = "cluster-vending-system" // CV_LEASE_NAMESPACE, from the downward API
		}
		if store == config.StoreDatabase {
			// From the Secret, in the cluster (database_dsn_patch.yaml).
			env[config.EnvDatabaseDSN] = "postgres://user@db.example.internal:5432/cluster_vending"
		}
		for name := range env {
			if !slices.Contains(envNames, name) {
				t.Errorf("%s: %s is not an environment variable of the serve command", overlay, name)
			}
		}
		cfg, err := config.LoadServe(lookup(nil), lookup(env), nil)
		if err != nil {
			t.Errorf("%s: the settings do not load: %v", overlay, err)
			continue
		}
		if cfg.Store != store || cfg.HTTPAddr != ":8080" || !cfg.LeaderElect || cfg.LeaseNamespace != "cluster-vending-system" {
			t.Errorf("%s: loaded store %s, addr %s, leader election %t in %s", overlay, cfg.Store, cfg.HTTPAddr,
				cfg.LeaderElect, cfg.LeaseNamespace)
		}
	}
}

// allows reports whether role lets verb be used on resource of group.
func allows(role *rbacv1.ClusterRole, group, resource, verb string) bool {
	return slices.ContainsFunc(role.Rules, func(r rbacv1.PolicyRule) bool {
		return slices.Contains(r.APIGroups, group) && slices.Contains(r.Verbs, verb) &&
			(slices.Contains(r.Resources, resource) || slices.Contains(r.Resources, "*"))
	})
}

// Both service accounts can do, on every managed resource kind the
// platform uses (the activation policy lists them), what the code does
// with them. provider-helm's Release is activated but not rendered yet.
func TestRolesCoverTheKindsThePlatformRenders(t *testing.T) {
	roles := map[string]*rbacv1.ClusterRole{
		"controller": clusterRole(t, controllerRole),
		"server":     clusterRole(t, filepath.Join(serverBase, "role.yaml")),
	}
	verbs := []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
	kinds := []string{"usages.protection.crossplane.io", "providerconfigs.aws.m.upbound.io"}
	for _, activation := range crossplaneaws.ActivationPolicy().Spec.Activations {
		if name := string(activation); !strings.HasSuffix(name, ".helm.m.crossplane.io") {
			kinds = append(kinds, name)
		}
	}
	for name, role := range roles {
		for _, kind := range kinds {
			resource, group, _ := strings.Cut(kind, ".")
			for _, verb := range verbs {
				if !allows(role, group, resource, verb) {
					t.Errorf("%s role: no %s on %s", name, verb, kind)
				}
			}
		}
		if !allows(role, "", "secrets", "get") || allows(role, "", "secrets", "list") || allows(role, "", "secrets", "create") {
			t.Errorf("%s role: Secrets must be readable by name and nothing more", name)
		}
		if !allows(role, "", "namespaces", "create") {
			t.Errorf("%s role: cannot create tenant namespaces", name)
		}
	}
	server := roles["server"]
	for _, need := range [][3]string{
		{"pkg.crossplane.io", "providers", "create"}, {"pkg.crossplane.io", "deploymentruntimeconfigs", "create"},
		{"apiextensions.crossplane.io", "managedresourceactivationpolicies", "update"},
		{"apiextensions.k8s.io", "customresourcedefinitions", "patch"},
		{"cloud.cluster-vending.io", "networks", "update"}, {"cloud.cluster-vending.io", "clusters", "update"},
	} {
		if !allows(server, need[0], need[1], need[2]) {
			t.Errorf("server role: no %s on %s.%s", need[2], need[1], need[0])
		}
	}
}
