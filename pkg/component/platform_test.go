package component_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

func entry(name string, requires ...string) component.PlatformEntry {
	return component.PlatformEntry{
		Definition: component.Definition{Name: name, Management: component.Platform, HasInstall: true, Requires: requires},
		Install:    component.Install{Namespace: name + "-system", Main: component.Chart{Repository: "https://charts.example.com", Name: name}},
	}
}

func platformInput(entries ...component.PlatformEntry) component.PlatformInput {
	pins := map[string]component.Pin{}
	for _, e := range entries {
		pins[e.Name] = component.Pin{Version: "1.0.0"}
	}
	return component.PlatformInput{
		Entries: entries, Release: "2026-11-a", Pins: pins,
		Cluster: component.Cluster{Name: "prod", IRSA: true},
		Vars: func(name string) component.Vars {
			return component.Vars{component.VarClusterName: "prod", component.VarRegion: "us-east-1",
				component.VarAccountID: "444455556666", component.VarIAMRoleName: "prod-" + name + "-role",
				component.VarSystemNodeSelector: `{"pool":"system"}`, component.VarSystemTolerations: `[]`}
		},
		Plan: crossplaneaws.PlatformPlan{Namespace: "tenant-acme", TenantID: "acme", ClusterID: "c-1", ClusterPrefix: "prod-1a2b3c4d"},
		IAM: crossplaneaws.ComponentIAMPlan{Namespace: "tenant-acme", ProviderConfigName: "aws", TenantID: "acme", ClusterID: "c-1",
			ClusterName: "prod", ClusterPrefix: "prod-1a2b3c4d", Region: "us-east-1"},
	}
}

func named(plan crossplaneaws.PlatformPlan, name string) (crossplaneaws.PlatformComponentPlan, bool) {
	for _, c := range plan.Components {
		if c.Name == name {
			return c, true
		}
	}
	return crossplaneaws.PlatformComponentPlan{}, false
}

// Versions come from the release's pins; the pin's values go over the
// definition's platform values; placeholders are filled in; a companion's
// values are the key named after it, taken out of the main chart's.
func TestPlanPlatformResolvesVersionsAndValues(t *testing.T) {
	e := entry("karpenter")
	e.PlatformValues = component.Values{
		"settings":            map[string]any{"clusterName": "${cluster.name}"},
		"nodeSelector":        "${system.nodeSelector}",
		"image":               map[string]any{"tag": "old"},
		"karpenter-nodeclass": map[string]any{"role": "${cluster.name}-node"},
	}
	crds := component.Chart{Repository: "oci://public.ecr.aws/karpenter", Name: "karpenter-crd"}
	e.Install.CRDs = &crds
	e.Install.Companions = []component.Companion{{Name: "karpenter-nodeclass", Chart: component.Chart{Repository: "oci://ghcr.io/x", Name: "karpenter-nodeclass"}}}
	in := platformInput(e)
	in.Pins["karpenter"] = component.Pin{Version: "1.15.1", Values: component.Values{"image": map[string]any{"tag": "new"}}}
	in.Pins["karpenter-nodeclass"] = component.Pin{Version: "0.1.0"}

	plan, skipped := component.PlanPlatform(in)

	if len(skipped) != 0 {
		t.Fatalf("skipped %v", skipped)
	}
	c, _ := named(plan, "karpenter")
	wantValues := map[string]any{
		"settings": map[string]any{"clusterName": "prod"}, "nodeSelector": map[string]any{"pool": "system"},
		"image": map[string]any{"tag": "new"},
	}
	if !reflect.DeepEqual(c.Main.Values, wantValues) {
		t.Errorf("main values %v, want %v", c.Main.Values, wantValues)
	}
	if c.Main.Chart.Version != "1.15.1" || !c.Main.SkipCRDs || c.Namespace != "karpenter-system" {
		t.Errorf("main %+v in %s", c.Main, c.Namespace)
	}
	if c.CRDs == nil || c.CRDs.Name != "karpenter-crds" || c.CRDs.Chart.Version != "1.15.1" || c.CRDs.Chart.Name != "karpenter-crd" || c.CRDs.Values != nil {
		t.Errorf("CRD chart %+v", c.CRDs)
	}
	if len(c.Companions) != 1 || c.Companions[0].Chart.Version != "0.1.0" ||
		!reflect.DeepEqual(c.Companions[0].Values, map[string]any{"role": "prod-node"}) {
		t.Errorf("companions %+v", c.Companions)
	}
	if _, kept := e.PlatformValues["karpenter-nodeclass"]; !kept {
		t.Error("the definition's values were changed")
	}
}

func TestPlanPlatformLeavesOut(t *testing.T) {
	private := entry("private")
	private.Install.Main.Private = true
	irsa := entry("needs-irsa")
	irsa.CloudResources = component.CloudResources{IAMRole: &component.IAMRole{Access: component.IRSA,
		ServiceAccounts: []component.ServiceAccount{{Namespace: "x", Name: "x"}}, ManagedPolicyARNs: []string{"arn:aws:iam::aws:policy/X"}}}
	bad := entry("bad-values")
	bad.PlatformValues = component.Values{"a": "${no.such}"}
	tests := []struct {
		name    string
		entries []component.PlatformEntry
		edit    func(*component.PlatformInput)
		want    map[string]string // left out -> part of the reason
		planned []string
	}{
		{name: "not pinned", entries: []component.PlatformEntry{entry("a"), entry("b")},
			edit:    func(in *component.PlatformInput) { delete(in.Pins, "a") },
			want:    map[string]string{"a": "not in release 2026-11-a"},
			planned: []string{"b"}},
		{name: "what requires a left-out component goes with it", entries: []component.PlatformEntry{entry("a"), entry("b", "a"), entry("c", "b")},
			edit: func(in *component.PlatformInput) { delete(in.Pins, "a") },
			want: map[string]string{"a": "not in release", "b": "requires a, which is not installed", "c": "requires b, which is not installed"}},
		{name: "private repository", entries: []component.PlatformEntry{private},
			want: map[string]string{"private": "credentials"}},
		{name: "IRSA off", entries: []component.PlatformEntry{irsa},
			edit: func(in *component.PlatformInput) { in.Cluster.IRSA = false },
			want: map[string]string{"needs-irsa": "spec.aws.irsa"}},
		{name: "unknown placeholder", entries: []component.PlatformEntry{bad},
			want: map[string]string{"bad-values": "no.such"}},
		{name: "render failure preserves dependents", entries: []component.PlatformEntry{bad, entry("dependent", "bad-values")},
			want: map[string]string{"bad-values": "no.such", "dependent": "requires bad-values"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := platformInput(tt.entries...)
			if tt.edit != nil {
				tt.edit(&in)
			}

			plan, skipped := component.PlanPlatform(in)

			if len(skipped) != len(tt.want) {
				t.Fatalf("left out %v, want %v", skipped, tt.want)
			}
			for _, s := range skipped {
				if want, ok := tt.want[s.Name]; !ok || !strings.Contains(s.Reason, want) {
					t.Errorf("%s left out because %q, want %q", s.Name, s.Reason, want)
				}
				intentionalRemoval := tt.name == "not pinned" || tt.name == "what requires a left-out component goes with it"
				if s.Preserve == intentionalRemoval {
					t.Errorf("%s: Preserve=%v, intentional removal=%v", s.Name, s.Preserve, intentionalRemoval)
				}
			}
			var planned []string
			for _, c := range plan.Components {
				planned = append(planned, c.Name)
			}
			if !reflect.DeepEqual(planned, tt.planned) {
				t.Errorf("planned %v, want %v", planned, tt.planned)
			}
		})
	}
}

// A placeholder without a value yet keeps the component in the plan,
// waiting; a companion the release does not pin is left out alone.
func TestPlanPlatformWaitsAndCompanions(t *testing.T) {
	e := entry("lb")
	e.PlatformValues = component.Values{"serviceAccount": map[string]any{"annotations": map[string]any{"role": "${iam.roleArn}"}}}
	e.CloudResources = component.CloudResources{IAMRole: &component.IAMRole{Access: component.PodIdentity,
		ServiceAccounts:   []component.ServiceAccount{{Namespace: "kube-system", Name: "lb"}},
		Policy:            `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:Describe*","Resource":"arn:aws:ec2:${region}:${account.id}:*"}]}`,
		ManagedPolicyARNs: []string{"arn:aws:iam::${account.id}:policy/extra"}}}
	e.Install.Companions = []component.Companion{{Name: "lb-extra", Chart: component.Chart{Name: "lb-extra"}}}
	in := platformInput(e)

	plan, skipped := component.PlanPlatform(in)

	c, ok := named(plan, "lb")
	if !ok || len(skipped) != 0 {
		t.Fatalf("plan %+v, skipped %v", plan.Components, skipped)
	}
	if c.WaitsFor != "${iam.roleArn}" {
		t.Errorf("WaitsFor = %q", c.WaitsFor)
	}
	if c.IAM == nil || c.IAM.Component != "lb" || c.IAM.Access != crossplaneaws.ComponentAccessPodIdentity ||
		!strings.Contains(c.IAM.Policy, "arn:aws:ec2:us-east-1:444455556666:*") ||
		!reflect.DeepEqual(c.IAM.ManagedPolicyARNs, []string{"arn:aws:iam::444455556666:policy/extra"}) ||
		len(c.IAM.ServiceAccounts) != 1 || c.IAM.ClusterPrefix != "prod-1a2b3c4d" {
		t.Errorf("IAM plan %+v", c.IAM)
	}
	if len(c.Companions) != 0 {
		t.Errorf("an unpinned companion was planned: %+v", c.Companions)
	}
	if got := component.MissingCompanions(e, in.Pins); !reflect.DeepEqual(got, []string{"lb-extra"}) {
		t.Errorf("MissingCompanions = %v", got)
	}

	// Once the role has an ARN the component no longer waits.
	vars := in.Vars
	in.Vars = func(name string) component.Vars {
		v := vars(name)
		v[component.VarIAMRoleARN] = "arn:aws:iam::444455556666:role/prod-lb-role"
		return v
	}
	plan, _ = component.PlanPlatform(in)
	if c, _ := named(plan, "lb"); c.WaitsFor != "" {
		t.Errorf("still waits for %q", c.WaitsFor)
	}
}

func TestInstallOf(t *testing.T) {
	def := &cloudv1alpha1.ComponentDefinition{Spec: cloudv1alpha1.ComponentDefinitionSpec{Namespace: "kube-system",
		Install: &cloudv1alpha1.ComponentInstall{Helm: &cloudv1alpha1.HelmInstall{
			HelmChartSource: cloudv1alpha1.HelmChartSource{Repository: "oci://r", Chart: "main", PlainHTTP: true},
			CRDs:            &cloudv1alpha1.HelmChartSource{Repository: "oci://r", Chart: "crds", Credentials: &cloudv1alpha1.HelmCredentials{}},
			Companions:      []cloudv1alpha1.HelmCompanion{{Name: "extra", HelmChartSource: cloudv1alpha1.HelmChartSource{Repository: "oci://r", Chart: "extra", InsecureSkipTLSVerify: true}}},
		}}}}

	got, ok := component.InstallOf(def)

	want := component.Install{Namespace: "kube-system", Main: component.Chart{Repository: "oci://r", Name: "main", PlainHTTP: true},
		CRDs:       &component.Chart{Repository: "oci://r", Name: "crds", Private: true},
		Companions: []component.Companion{{Name: "extra", Chart: component.Chart{Repository: "oci://r", Name: "extra", InsecureSkipTLSVerify: true}}}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("InstallOf = %+v, %v\nwant %+v", got, ok, want)
	}
	if _, ok := component.InstallOf(&cloudv1alpha1.ComponentDefinition{}); ok {
		t.Error("a bundle has no install")
	}
}

// The list a tenant reads: Platform components read-only and without an
// ID, the tenant's own, by name; inputs untouched.
func TestInstalled(t *testing.T) {
	platform := []component.InstalledComponent{{Name: "karpenter", Version: "1.15.1", Phase: "COMPONENT_PHASE_READY", ID: "ignored"},
		{Name: "aws-load-balancer-controller", Phase: "COMPONENT_PHASE_INSTALLING", Message: "waiting"}}
	tenant := []component.InstalledComponent{{ID: "id-1", Name: "gpu-operator", Version: "v26.7.2"},
		{ID: "id-2", Name: "karpenter"}}

	got := component.Installed(platform, tenant)

	want := []component.InstalledComponent{
		{Name: "aws-load-balancer-controller", Phase: "COMPONENT_PHASE_INSTALLING", Message: "waiting", Management: component.Platform},
		{ID: "id-1", Name: "gpu-operator", Version: "v26.7.2", Management: component.Tenant},
		{Name: "karpenter", Version: "1.15.1", Phase: "COMPONENT_PHASE_READY", Management: component.Platform},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Installed =\n%+v\nwant\n%+v", got, want)
	}
	if platform[0].ID != "ignored" || tenant[0].Management != "" {
		t.Error("the inputs were changed")
	}
}

// The shipped pin values decode; kept here so PinValues stays usable for
// the reconcile path.
func TestPinValuesForPlatform(t *testing.T) {
	v, err := component.PinValues(&runtime.RawExtension{Raw: []byte(`{"image":{"tag":"v1.37.0"}}`)})
	if err != nil || !reflect.DeepEqual(v, component.Values{"image": map[string]any{"tag": "v1.37.0"}}) {
		t.Errorf("PinValues = %v, %v", v, err)
	}
}

// No component is held for a node group: components follow sequence and
// requires only (PlatformComponentPlan.WaitsFor is for placeholders).
func TestPlanPlatformDoesNotHoldForANodeGroup(t *testing.T) {
	first, second := entry("karpenter"), entry("lb")
	first.Sequence, second.Sequence = 0, 100
	in := platformInput(first, second)

	plan, _ := component.PlanPlatform(in)

	for _, name := range []string{"karpenter", "lb"} {
		if c, _ := named(plan, name); c.WaitsFor != "" {
			t.Errorf("%s waits for %q", name, c.WaitsFor)
		}
	}
	if lb, _ := named(plan, "lb"); lb.Sequence != 100 {
		t.Errorf("lb sequence %d", lb.Sequence)
	}
}

// The definition's install timeout reaches every chart of the component;
// without one the charts carry none (the renderer's default applies).
func TestPlanPlatformCarriesTheInstallTimeout(t *testing.T) {
	e := entry("gpu-operator")
	e.Install.Timeout = 15 * time.Minute
	crds := component.Chart{Repository: "https://charts.example.com", Name: "gpu-operator-crd"}
	e.Install.CRDs = &crds
	e.Install.Companions = []component.Companion{{Name: "gpu-extra", Chart: component.Chart{Repository: "https://charts.example.com", Name: "gpu-extra"}}}
	in := platformInput(e)
	in.Pins["gpu-extra"] = component.Pin{Version: "0.1.0"}
	plain := platformInput(entry("kyverno"))

	plan, _ := component.PlanPlatform(in)
	unset, _ := component.PlanPlatform(plain)

	c, _ := named(plan, "gpu-operator")
	for name, got := range map[string]time.Duration{"main": c.Main.Timeout, "crds": c.CRDs.Timeout, "companion": c.Companions[0].Timeout} {
		if got != 15*time.Minute {
			t.Errorf("%s chart timeout %s, want 15m", name, got)
		}
	}
	if k, _ := named(unset, "kyverno"); k.Main.Timeout != 0 {
		t.Errorf("no install.timeout: %s", k.Main.Timeout)
	}
}

func TestInstallOfReadsTheTimeout(t *testing.T) {
	def := &cloudv1alpha1.ComponentDefinition{Spec: cloudv1alpha1.ComponentDefinitionSpec{Namespace: "x",
		Install: &cloudv1alpha1.ComponentInstall{Helm: &cloudv1alpha1.HelmInstall{HelmChartSource: cloudv1alpha1.HelmChartSource{
			Repository: "https://r", Chart: "c"}}, Timeout: &metav1.Duration{Duration: 20 * time.Minute}}}}

	got, ok := component.InstallOf(def)

	if !ok || got.Timeout != 20*time.Minute {
		t.Errorf("%v %+v", ok, got)
	}
	def.Spec.Install.Timeout = nil
	if got, _ := component.InstallOf(def); got.Timeout != 0 {
		t.Errorf("unset: %s", got.Timeout)
	}
}
