package config_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// The Helm chart (charts/cluster-vending) deploys what config/ deploys: the
// tests below render it with `helm template` (skipped without helm) and hold
// it to the kustomize manifests and to the commands' settings.

const (
	chartDir       = "../charts/cluster-vending"
	chartNamespace = "cluster-vending-system"
	chartRoleARN   = "arn:aws:iam::111122223333:role/crossplane-provider-aws"
)

// chartRoleCopies are the role files the chart reads (files/rbac, make
// helm-sync) and the config/ files they are copies of.
var chartRoleCopies = map[string]string{
	"role.yaml":                      "rbac/role.yaml",
	"platform_role.yaml":             "rbac/platform_role.yaml",
	"kubeconfig_reader_role.yaml":    "rbac/kubeconfig_reader_role.yaml",
	"usage_history_writer_role.yaml": "rbac/usage_history_writer_role.yaml",
	"leader_election_role.yaml":      "rbac/leader_election_role.yaml",
	"cluster_admin_role.yaml":        "rbac/cluster_admin_role.yaml",
	"cluster_editor_role.yaml":       "rbac/cluster_editor_role.yaml",
	"cluster_viewer_role.yaml":       "rbac/cluster_viewer_role.yaml",
	"network_admin_role.yaml":        "rbac/network_admin_role.yaml",
	"network_editor_role.yaml":       "rbac/network_editor_role.yaml",
	"network_viewer_role.yaml":       "rbac/network_viewer_role.yaml",
	"cloudaccount_admin_role.yaml":   "rbac/cloudaccount_admin_role.yaml",
	"cloudaccount_editor_role.yaml":  "rbac/cloudaccount_editor_role.yaml",
	"cloudaccount_viewer_role.yaml":  "rbac/cloudaccount_viewer_role.yaml",
	"release_admin_role.yaml":        "rbac/release_admin_role.yaml",
	"release_viewer_role.yaml":       "rbac/release_viewer_role.yaml",
	"server_role.yaml":               filepath.Join(serverBase, "role.yaml"),
	"live_role.yaml":                 "live/role.yaml",
}

// rendered is a `helm template` of the chart, by kind and name.
type rendered struct {
	deployments map[string]*appsv1.Deployment
	roles       map[string]*rbacv1.ClusterRole
	accounts    map[string]*corev1.ServiceAccount
	configMaps  map[string]*corev1.ConfigMap
	kinds       []string
}

// deployment is the rendered Deployment of that name.
func (r rendered) deployment(t *testing.T, name string) *appsv1.Deployment {
	t.Helper()
	if d, ok := r.deployments[name]; ok {
		return d
	}
	t.Fatalf("the chart renders no Deployment %s", name)
	return nil
}

// The chart's role files are the config/ files, unchanged: `make helm-sync`
// copies them.
func TestChartRoleFilesAreCopiesOfConfig(t *testing.T) {
	for copied, source := range chartRoleCopies {
		got, err := os.ReadFile(filepath.Join(chartDir, "files", "rbac", copied))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Clean(source))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("charts/cluster-vending/files/rbac/%s differs from config/%s: run make helm-sync", copied, source)
		}
	}
}

// Every ClusterRole the chart renders has the rules of its config/ role;
// the roles the controller binds by name keep those names.
func TestChartRBACMatchesConfig(t *testing.T) {
	r := renderChart(t)
	for copied, source := range chartRoleCopies {
		want := roleFile(t, source)
		if want.Kind != "ClusterRole" {
			continue
		}
		name := "cluster-vending-" + want.Name
		got, ok := r.roles[name]
		if !ok {
			t.Errorf("the chart renders no ClusterRole %s (files/rbac/%s)", name, copied)
			continue
		}
		if !reflect.DeepEqual(got.Rules, want.Rules) {
			t.Errorf("ClusterRole %s: rules differ from config/%s", name, source)
		}
	}
	for _, name := range []string{reconcile.KubeconfigReaderRole, reconcile.UsageHistoryWriterRole} {
		if _, ok := r.roles[name]; !ok {
			t.Errorf("the chart renders no ClusterRole %s, the name the controller binds", name)
		}
	}
	if _, ok := r.accounts[reconcile.LiveReaderServiceAccount]; !ok {
		t.Errorf("the chart renders no service account %s, the one the controller binds for the live reader", reconcile.LiveReaderServiceAccount)
	}
	if slices.Contains(r.kinds, "CustomResourceDefinition") {
		t.Error("the chart renders CRDs; the controller installs them (README.md)")
	}
}

// roleFile reads a Role or ClusterRole manifest.
func roleFile(t *testing.T, path string) *rbacv1.ClusterRole {
	t.Helper()
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(bytes.TrimPrefix(documents(t, path)[0], []byte("---\n")), role); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return role
}

// renderChart runs helm template with the release name cluster-vending, the
// placeholder management role and the given --set values, and keys the
// objects by name. The test is skipped without helm.
func renderChart(t *testing.T, set ...string) rendered {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	args := []string{"template", "cluster-vending", chartDir, "--namespace", chartNamespace, "--set", "aws.managementRoleArn=" + chartRoleARN}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	out, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	r := rendered{deployments: map[string]*appsv1.Deployment{}, roles: map[string]*rbacv1.ClusterRole{},
		accounts: map[string]*corev1.ServiceAccount{}, configMaps: map[string]*corev1.ConfigMap{}}
	for _, doc := range bytes.Split(out, []byte("\n---\n")) {
		var meta struct{ Kind string }
		if yaml.Unmarshal(doc, &meta) != nil || meta.Kind == "" {
			continue
		}
		r.kinds = append(r.kinds, meta.Kind)
		var err error
		switch meta.Kind {
		case "Deployment":
			d := &appsv1.Deployment{}
			err = yaml.UnmarshalStrict(doc, d)
			r.deployments[d.Name] = d
		case "ClusterRole":
			c := &rbacv1.ClusterRole{}
			err = yaml.UnmarshalStrict(doc, c)
			r.roles[c.Name] = c
		case "ServiceAccount":
			s := &corev1.ServiceAccount{}
			err = yaml.UnmarshalStrict(doc, s)
			r.accounts[s.Name] = s
		case "ConfigMap":
			c := &corev1.ConfigMap{}
			err = yaml.UnmarshalStrict(doc, c)
			r.configMaps[c.Name] = c
		}
		if err != nil {
			t.Fatalf("rendered %s: %v\n%s", meta.Kind, err, doc)
		}
	}
	return r
}

// platformFile writes the rendered settings file and loads it.
func platformFile(t *testing.T, r rendered) (config.LookupFunc, string) {
	t.Helper()
	cm, ok := r.configMaps["cluster-vending-platform"]
	if !ok {
		t.Fatal("the chart renders no cluster-vending-platform ConfigMap")
	}
	path := filepath.Join(t.TempDir(), "platform.yaml")
	if err := os.WriteFile(path, []byte(cm.Data["platform.yaml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("the chart's settings file does not load: %v", err)
	}
	return file, path
}

// containerEnv resolves a container's env, the downward API as the pod in
// the chart's namespace would.
func containerEnv(t *testing.T, c corev1.Container, envNames []string, sa string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, e := range c.Env {
		if !slices.Contains(envNames, e.Name) {
			t.Errorf("%s: %s is not an environment variable of the command", c.Name, e.Name)
		}
		if strings.HasPrefix(e.Name, "AWS_") {
			t.Errorf("%s: %s: AWS keys do not belong in a manifest", c.Name, e.Name)
		}
		env[e.Name] = e.Value
		if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
			env[e.Name] = map[string]string{"metadata.namespace": chartNamespace, "spec.serviceAccountName": sa}[e.ValueFrom.FieldRef.FieldPath]
		}
	}
	for _, name := range nodeGroupVariables() {
		if _, set := env[name]; set {
			t.Errorf("%s sets %s: it would override the shared file", c.Name, name)
		}
	}
	return env
}

// The controller the chart deploys loads with the chart's settings, as the
// one of config/manager does: leader election in its namespace, its own
// service account, the management role, providers installed.
func TestChartControllerSettingsLoad(t *testing.T) {
	r := renderChart(t)
	file, _ := platformFile(t, r)
	d := r.deployment(t, "cluster-vending-controller")
	pod := d.Spec.Template.Spec
	c := pod.Containers[0]
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
	if args["config"] != platformConfigPath {
		t.Errorf("--config = %q, want %s", args["config"], platformConfigPath)
	}
	env := containerEnv(t, c, envNames, pod.ServiceAccountName)
	cfg, err := config.LoadController(lookup(args), lookup(env), file)
	if err != nil {
		t.Fatalf("the chart's controller settings do not load: %v", err)
	}
	account := config.ServiceAccountRef{Namespace: chartNamespace, Name: "cluster-vending-controller"}
	if cfg.ServiceAccount == nil || *cfg.ServiceAccount != account {
		t.Errorf("service account = %+v, want %+v", cfg.ServiceAccount, account)
	}
	if !cfg.LeaderElect || cfg.LeaseNamespace != chartNamespace || cfg.ProbeAddr != ":8081" || cfg.MetricsAddr != "0" {
		t.Errorf("loaded %+v", cfg)
	}
	if cfg.AWS.ManagementRoleARN != chartRoleARN || !cfg.InstallProviders {
		t.Errorf("the controller's AWS settings: %+v, install providers %t", cfg.AWS, cfg.InstallProviders)
	}
}

// The API server the chart deploys passes only serve settings, nothing of
// reconciling, and reaches the live reader by its Service.
func TestChartServerSettingsLoad(t *testing.T) {
	r := renderChart(t)
	file, _ := platformFile(t, r)
	pod := r.deployment(t, "cluster-vending-server").Spec.Template.Spec
	c := pod.Containers[0]
	if !slices.Equal(c.Command, []string{"/cluster-vending", "serve"}) || !slices.Equal(c.Args, []string{"--config=" + platformConfigPath}) {
		t.Errorf("command %v args %v", c.Command, c.Args)
	}
	_, envNames := known(config.ServeSettings(), config.GlobalSettings())
	env := containerEnv(t, c, envNames, pod.ServiceAccountName)
	for name := range env {
		if strings.Contains(name, "AWS") || strings.Contains(name, "LEADER") || strings.Contains(name, "PROVIDER") {
			t.Errorf("the server is given %s: it reconciles nothing", name)
		}
	}
	if want := "http://cluster-vending-live." + chartNamespace + ".svc:8090"; env[config.EnvLiveURL] != want {
		t.Errorf("%s = %q, want %q", config.EnvLiveURL, env[config.EnvLiveURL], want)
	}
	if _, set := env[config.EnvLiveAllowWithoutAuth]; set {
		t.Errorf("the chart sets %s", config.EnvLiveAllowWithoutAuth)
	}
	cfg, err := config.LoadServe(lookup(nil), lookup(env), file)
	if err != nil {
		t.Fatalf("the chart's server settings do not load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("addr %s", cfg.HTTPAddr)
	}
	// The server never gets the Secrets of workload clusters.
	if allows(r.roles["cluster-vending-server-role"], "", "secrets", "get") {
		t.Error("the server role may get Secrets")
	}
}

// The live reader: one replica, Recreate, its fixed service account, only
// live settings.
func TestChartLiveReader(t *testing.T) {
	r := renderChart(t)
	d := r.deployment(t, "cluster-vending-live")
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("live reader: replicas %v, strategy %q; want one, Recreate", d.Spec.Replicas, d.Spec.Strategy.Type)
	}
	pod := d.Spec.Template.Spec
	if pod.ServiceAccountName != reconcile.LiveReaderServiceAccount {
		t.Errorf("live reader runs as %s, the controller binds %s", pod.ServiceAccountName, reconcile.LiveReaderServiceAccount)
	}
	_, envNames := known(config.GlobalSettings(), config.LiveSettings())
	env := containerEnv(t, pod.Containers[0], envNames, pod.ServiceAccountName)
	if _, err := config.LoadLive(lookup(nil), lookup(env), nil); err != nil {
		t.Errorf("the live reader's settings do not load: %v", err)
	}
}

// The chart's default settings file is config/node-groups/platform.yaml
// plus the management role.
func TestChartPlatformDefaultsMatchConfig(t *testing.T) {
	r := renderChart(t)
	var got, want map[string]any
	if err := yaml.Unmarshal([]byte(r.configMaps["cluster-vending-platform"].Data["platform.yaml"]), &got); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("node-groups", "platform.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if got["aws-management-role-arn"] != chartRoleARN {
		t.Errorf("aws-management-role-arn = %v", got["aws-management-role-arn"])
	}
	delete(got, "aws-management-role-arn")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the chart's platform default differs from config/node-groups/platform.yaml:\n got %v\nwant %v", got, want)
	}
	for _, name := range []string{"cluster-vending-server", "cluster-vending-controller"} {
		pod := r.deployment(t, name).Spec.Template
		if pod.Annotations["checksum/platform"] == "" {
			t.Errorf("%s has no checksum/platform annotation: a settings change would not roll it", name)
		}
		mounted := slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool {
			return v.ConfigMap != nil && v.ConfigMap.Name == "cluster-vending-platform"
		})
		if !mounted {
			t.Errorf("%s does not mount cluster-vending-platform", name)
		}
	}
}

// No service account has an IAM role by default; only the controller's and
// the server's may be given one.
func TestChartServiceAccountsHaveNoDefaultRole(t *testing.T) {
	r := renderChart(t)
	for name, sa := range r.accounts {
		if _, set := sa.Annotations["eks.amazonaws.com/role-arn"]; set {
			t.Errorf("service account %s has an IAM role by default", name)
		}
	}
}
