// Package config_test keeps the deploy manifests (config/server and
// config) in step with the code they run: the settings they
// pass are settings the commands accept, and the RBAC covers the kinds the
// platform renders. It reads the manifests as files; no kustomize, Docker
// or cluster is needed.
package config_test

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
	controllerDeployment = "manager/manager.yaml"
	controllerRole       = "rbac/platform_role.yaml"
	serverBase           = "server/base"
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
		if strings.HasPrefix(e.Name, "AWS_") {
			t.Errorf("%s: AWS keys do not belong in a manifest; the controller's credentials are its service account's role", e.Name)
		}
		env[e.Name] = e.Value
		if e.ValueFrom != nil {
			env[e.Name] = "cluster-vending-system" // the namespace, from the downward API
		}
	}
	cfg, err := config.LoadController(lookup(args), lookup(env), nil)
	if err != nil {
		t.Fatalf("the Deployment's settings do not load: %v", err)
	}
	if !cfg.LeaderElect || cfg.LeaseNamespace != "cluster-vending-system" || cfg.ProbeAddr != ":8081" || cfg.MetricsAddr != "0" {
		t.Errorf("loaded %+v", cfg)
	}
	// The controller is what reconciles: it has the management account and
	// installs the providers.
	if cfg.AWS.ManagementAccountID == "" || cfg.AWS.ManagementRoleARN == "" || !cfg.InstallProviders {
		t.Errorf("the controller's AWS settings: %+v, install providers %t", cfg.AWS, cfg.InstallProviders)
	}
	// The metrics component replaces this argument by position.
	if c.Args[2] != "--metrics-addr=0" {
		t.Errorf("args[2] = %s; config/metrics patches --metrics-addr there", c.Args[2])
	}
}

// The server's settings are settings the serve command has, and only
// its three: no store, loop, leader election, provider or AWS setting.
func TestServerSettingsLoad(t *testing.T) {
	_, envNames := known(config.ServeSettings(), config.GlobalSettings())
	c := deployment(t, filepath.Join(serverBase, "deployment.yaml")).Spec.Template.Spec.Containers[0]
	if !slices.Equal(c.Command, []string{"/cluster-vending", "serve"}) || len(c.Args) != 0 || len(c.Env) != 0 {
		t.Errorf("command %v args %v env %v", c.Command, c.Args, c.Env)
	}
	env := literals(t, serverBase)
	for name := range env {
		if !slices.Contains(envNames, name) {
			t.Errorf("%s is not an environment variable of the serve command", name)
		}
		if strings.Contains(name, "AWS") || strings.Contains(name, "LEADER") || strings.Contains(name, "PROVIDER") {
			t.Errorf("the server is given %s: it reconciles nothing and makes no AWS call", name)
		}
	}
	cfg, err := config.LoadServe(lookup(nil), lookup(env), nil)
	if err != nil {
		t.Fatalf("the settings do not load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("loaded addr %s", cfg.HTTPAddr)
	}
}

// serviceAccountRole is the IAM role annotation a kustomization patches
// onto its ServiceAccount; empty for none.
func serviceAccountRole(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const annotation = "eks.amazonaws.com/role-arn:"
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, annotation) {
			return strings.TrimSpace(strings.TrimPrefix(line, annotation))
		}
	}
	return ""
}

// Only what looks at AWS has an IAM role: the controller's service
// account (IRSA), never the API server's. The role in the manifests is a
// placeholder, not a real account.
func TestOnlyTheControllerHasAnIAMRole(t *testing.T) {
	if role := serviceAccountRole(t, "default"); !strings.HasPrefix(role, "arn:aws:iam::111122223333:role/") {
		t.Errorf("the controller's service account role = %q, want the placeholder IRSA role", role)
	}
	for _, dir := range []string{serverBase} {
		if role := serviceAccountRole(t, dir); role != "" {
			t.Errorf("%s gives the server an IAM role (%s): the server makes no AWS call", dir, role)
		}
	}
	for _, file := range []string{"rbac/service_account.yaml", filepath.Join(serverBase, "service_account.yaml")} {
		raw, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, "role-arn") {
				t.Errorf("%s sets an IAM role itself; it is set per deployment (config/default)", file)
			}
		}
	}
}

// The release objects: the operator writes them (release-admin), anyone
// may read them (release-viewer), the controller reads them and writes
// their status and the release finalizer, the API server only reads them.
func TestReleaseObjectRoles(t *testing.T) {
	kinds := []string{"releases", "releasechannels", "releaseholds"}
	const group = "cloud.cluster-vending.io"
	admin, viewer := clusterRole(t, "rbac/release_admin_role.yaml"), clusterRole(t, "rbac/release_viewer_role.yaml")
	controller, server := clusterRole(t, "rbac/role.yaml"), clusterRole(t, filepath.Join(serverBase, "role.yaml"))
	for _, kind := range kinds {
		for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
			if !allows(admin, group, kind, verb) {
				t.Errorf("release-admin: no %s on %s", verb, kind)
			}
		}
		for _, verb := range []string{"get", "list", "watch"} {
			for name, role := range map[string]*rbacv1.ClusterRole{"release-viewer": viewer, "controller": controller, "server": server} {
				if !allows(role, group, kind, verb) {
					t.Errorf("%s: no %s on %s", name, verb, kind)
				}
			}
		}
		for _, verb := range []string{"create", "update", "patch", "delete"} {
			for name, role := range map[string]*rbacv1.ClusterRole{"release-viewer": viewer, "server": server} {
				if allows(role, group, kind, verb) {
					t.Errorf("%s: may %s %s; only the operator writes release objects", name, verb, kind)
				}
			}
		}
		if allows(admin, group, kind+"/status", "update") {
			t.Errorf("release-admin: may update %s/status; status is the controller's", kind)
		}
		if !allows(controller, group, kind+"/status", "update") {
			t.Errorf("controller: cannot update %s/status", kind)
		}
		if allows(controller, group, kind, "delete") {
			t.Errorf("controller: may delete %s; only the operator deletes release objects", kind)
		}
	}
	if !allows(controller, group, "releases/finalizers", "update") || !allows(controller, group, "releases", "update") {
		t.Error("controller: cannot keep the finalizer on releases")
	}
}

// The component kinds: the operator writes the catalog (release-admin),
// anyone may read it, the controller and the API server only read it; the
// API server writes tenants' Components, the controller reconciles them
// (status and finalizer). The operator never writes their status.
func TestComponentRoles(t *testing.T) {
	const group = "cloud.cluster-vending.io"
	admin, viewer := clusterRole(t, "rbac/release_admin_role.yaml"), clusterRole(t, "rbac/release_viewer_role.yaml")
	controller, server := clusterRole(t, "rbac/role.yaml"), clusterRole(t, filepath.Join(serverBase, "role.yaml"))
	writes := []string{"create", "update", "patch", "delete"}
	reads := []string{"get", "list", "watch"}
	for _, verb := range append(reads, writes...) {
		if !allows(admin, group, "componentdefinitions", verb) {
			t.Errorf("release-admin: no %s on componentdefinitions", verb)
		}
		if !allows(server, group, "components", verb) {
			t.Errorf("server: no %s on components", verb)
		}
		if !allows(controller, group, "components", verb) {
			t.Errorf("controller: no %s on components", verb)
		}
	}
	for _, verb := range reads {
		for name, role := range map[string]*rbacv1.ClusterRole{"release-viewer": viewer, "controller": controller, "server": server} {
			if !allows(role, group, "componentdefinitions", verb) {
				t.Errorf("%s: no %s on componentdefinitions", name, verb)
			}
		}
	}
	for _, verb := range writes {
		for name, role := range map[string]*rbacv1.ClusterRole{"release-viewer": viewer, "controller": controller, "server": server} {
			if allows(role, group, "componentdefinitions", verb) {
				t.Errorf("%s: may %s componentdefinitions; only the operator writes the catalog", name, verb)
			}
		}
		for name, role := range map[string]*rbacv1.ClusterRole{"release-admin": admin, "release-viewer": viewer} {
			if allows(role, group, "components", verb) {
				t.Errorf("%s: may %s components; they are tenants' objects", name, verb)
			}
		}
	}
	for _, kind := range []string{"components", "componentdefinitions"} {
		if !allows(controller, group, kind+"/status", "update") {
			t.Errorf("controller: cannot update %s/status", kind)
		}
		if allows(admin, group, kind+"/status", "update") {
			t.Errorf("release-admin: may update %s/status; status is the controller's", kind)
		}
	}
	if !allows(controller, group, "components/finalizers", "update") {
		t.Error("controller: cannot keep the finalizer on components")
	}
}

// allows reports whether role lets verb be used on resource of group.
func allows(role *rbacv1.ClusterRole, group, resource, verb string) bool {
	return slices.ContainsFunc(role.Rules, func(r rbacv1.PolicyRule) bool {
		return slices.Contains(r.APIGroups, group) && slices.Contains(r.Verbs, verb) &&
			(slices.Contains(r.Resources, resource) || slices.Contains(r.Resources, "*"))
	})
}

// managedKinds are the kinds the platform renders or cleans up, as
// "resource.group": what the activation policy lists (provider-helm's
// Release among them: the controller releases stuck ones when a cluster is
// deleted), Usages, the accounts' AWS ProviderConfigs and the clusters'
// provider-helm ProviderConfigs.
func managedKinds() []string {
	kinds := []string{"usages.protection.crossplane.io", "providerconfigs.aws.m.upbound.io", "providerconfigs.helm.m.crossplane.io"}
	for _, activation := range crossplaneaws.ActivationPolicy().Spec.Activations {
		kinds = append(kinds, string(activation))
	}
	return kinds
}

// What reconciles can do, on every managed resource kind the platform
// uses, what the code does with them, and can install the providers: the
// controller. It reads no Secret.
func TestReconcilerRolesCoverTheKindsThePlatformRenders(t *testing.T) {
	roles := map[string]*rbacv1.ClusterRole{
		"controller": clusterRole(t, controllerRole),
	}
	verbs := []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
	for name, role := range roles {
		for _, kind := range managedKinds() {
			resource, group, _ := strings.Cut(kind, ".")
			for _, verb := range verbs {
				if !allows(role, group, resource, verb) {
					t.Errorf("%s role: no %s on %s", name, verb, kind)
				}
			}
		}
		for _, need := range [][3]string{
			{"pkg.crossplane.io", "providers", "create"}, {"pkg.crossplane.io", "deploymentruntimeconfigs", "create"},
			{"apiextensions.crossplane.io", "managedresourceactivationpolicies", "update"}, {"", "namespaces", "create"},
		} {
			if !allows(role, need[0], need[1], need[2]) {
				t.Errorf("%s role: no %s on %s.%s", name, need[2], need[1], need[0])
			}
		}
		for _, verb := range verbs {
			if allows(role, "", "secrets", verb) {
				t.Errorf("%s role: may %s Secrets; the reconcilers read none (their AWS access is their own role)", name, verb)
			}
		}
	}
	if !allows(roles["controller"], "apiextensions.k8s.io", "customresourcedefinitions", "patch") {
		t.Error("controller role: cannot install the CRDs of the platform's kinds")
	}
}

// The API server stores the desired state and reads: it writes its own
// kinds and tenant namespaces, and can neither change in place nor delete
// anything of a network or cluster, install a provider or a CRD, or read
// a Secret.
func TestServerRoleOnlyStoresAndReads(t *testing.T) {
	server := clusterRole(t, filepath.Join(serverBase, "role.yaml"))
	for _, need := range [][3]string{
		{"cloud.cluster-vending.io", "networks", "update"}, {"cloud.cluster-vending.io", "clusters", "delete"},
		{"cloud.cluster-vending.io", "cloudaccounts", "create"}, {"", "namespaces", "create"},
	} {
		if !allows(server, need[0], need[1], need[2]) {
			t.Errorf("server role: no %s on %s.%s", need[2], need[1], need[0])
		}
	}
	for _, kind := range managedKinds() {
		resource, group, _ := strings.Cut(kind, ".")
		for _, verb := range []string{"get", "list", "watch"} {
			if !allows(server, group, resource, verb) {
				t.Errorf("server role: cannot %s %s (status and request checks read it)", verb, kind)
			}
		}
		for _, verb := range []string{"update", "delete", "deletecollection"} {
			if allows(server, group, resource, verb) {
				t.Errorf("server role: may %s %s; only what reconciles does", verb, kind)
			}
		}
	}
	for _, forbidden := range [][2]string{
		{"pkg.crossplane.io", "providers"}, {"pkg.crossplane.io", "deploymentruntimeconfigs"},
		{"apiextensions.crossplane.io", "managedresourceactivationpolicies"}, {"apiextensions.k8s.io", "customresourcedefinitions"},
		{"", "secrets"},
	} {
		for _, verb := range []string{"get", "list", "create", "update", "patch", "delete"} {
			if allows(server, forbidden[0], forbidden[1], verb) {
				t.Errorf("server role: may %s %s.%s", verb, forbidden[1], forbidden[0])
			}
		}
	}
	if allows(server, "", "namespaces", "delete") {
		t.Error("server role: may delete namespaces")
	}
}
