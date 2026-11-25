package controller

import (
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// metricsAgent is a made-up ordinary platform component, as a platform
// team would add it: a definition file and a release pin, no Go.
const metricsAgent = `apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ComponentDefinition
metadata:
  name: metrics-agent
spec:
  provider: aws
  description: A made-up metrics agent on the platform pool.
  management: Platform
  namespace: metrics
  requires: [kyverno]
  install:
    helm:
      repository: https://charts.example.com
      chart: metrics-agent
  sizing:
    deployments:
      - name: metrics-agent
        replicas: replicaCount
        requests: resources.requests
  platformValues:
    replicaCount: 2
    resources:
      requests: {cpu: 100m, memory: 128Mi}
    nodeSelector: ${platform.nodeSelector}
    tolerations: ${platform.tolerations}
`

func decodeDefinition(t *testing.T, raw string) *cloudv1alpha1.ComponentDefinition {
	t.Helper()
	d := &cloudv1alpha1.ComponentDefinition{}
	if err := yaml.UnmarshalStrict([]byte(raw), d); err != nil {
		t.Fatal(err)
	}
	return d
}

// withPin is rel with every minor pinning name at version.
func withPin(rel *cloudv1alpha1.Release, name, version string) *cloudv1alpha1.Release {
	out := rel.DeepCopy()
	for i := range out.Spec.Content.Kubernetes {
		out.Spec.Content.Kubernetes[i].Components = append(out.Spec.Content.Kubernetes[i].Components, cloudv1alpha1.ComponentPin{Name: name, Version: version})
	}
	return out
}

// renamed is the shipped definition file as another name: its metadata
// name and its release pins.
func renamed(t *testing.T, file, from, to string, rel *cloudv1alpha1.Release) (*cloudv1alpha1.ComponentDefinition, *cloudv1alpha1.Release) {
	t.Helper()
	raw, err := catalog.Raw(file)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), "metadata:\n  name: "+from+"\n", "metadata:\n  name: "+to+"\n", 1)
	if text == string(raw) {
		t.Fatalf("%s has no metadata name %s", file, from)
	}
	out := rel.DeepCopy()
	for i := range out.Spec.Content.Kubernetes {
		for j := range out.Spec.Content.Kubernetes[i].Components {
			if pin := &out.Spec.Content.Kubernetes[i].Components[j]; pin.Name == from {
				pin.Name = to
			}
		}
	}
	return decodeDefinition(t, text), out
}

// checkedSet is defs and rel as `controller --check` reads them, and what it
// reports with the default node groups.
func checkedSet(t *testing.T, defs []*cloudv1alpha1.ComponentDefinition, rel *cloudv1alpha1.Release) []string {
	t.Helper()
	var set catalog.Set
	for _, d := range defs {
		d = d.DeepCopy()
		d.Annotations = map[string]string{catalog.SchemaAnnotation: catalog.SchemaVersion}
		set.Definitions = append(set.Definitions, *d)
	}
	rel = rel.DeepCopy()
	one := int64(1)
	rel.Spec.Serial = &one
	rel.Annotations = map[string]string{catalog.SchemaAnnotation: catalog.SchemaVersion}
	set.Releases = append(set.Releases, *rel)
	for _, ch := range DefaultChannels() {
		ch.Annotations = map[string]string{catalog.SchemaAnnotation: catalog.SchemaVersion}
		set.Channels = append(set.Channels, *ch)
	}
	group, platform := config.DefaultNodeGroups().System, config.DefaultNodeGroups().Platform
	var out []string
	for _, p := range catalog.Check(set, catalog.Options{RenderVersion: crossplaneaws.RenderVersion, SystemGroup: &group, PlatformPool: &platform}) {
		out = append(out, p.String())
	}
	return out
}

// planOf plans defs under rel's first minor, NaaS on or off, and returns
// the planned component names in install order.
func planOf(t *testing.T, defs []*cloudv1alpha1.ComponentDefinition, rel *cloudv1alpha1.Release, naas bool) []string {
	t.Helper()
	items := make([]cloudv1alpha1.ComponentDefinition, 0, len(defs))
	for _, d := range defs {
		items = append(items, *d)
	}
	entries, err := component.PlatformEntries(items)
	if err != nil {
		t.Fatal(err)
	}
	k := rel.Spec.Content.Kubernetes[0]
	pins := map[string]component.Pin{}
	for _, c := range k.Components {
		values, err := component.PinValues(c.Values)
		if err != nil {
			t.Fatal(err)
		}
		pins[c.Name] = component.Pin{Version: c.Version, Values: values}
	}
	in := component.PlatformInput{Entries: component.ForNaaS(entries, naas), Release: rel.Name, Pins: pins,
		Cluster: component.Cluster{Name: "prod", Running: true, KubernetesMinor: k.Minor, IRSA: true},
		Vars:    shippedVars,
		Plan: crossplaneaws.PlatformPlan{Namespace: "tenant-acme", TenantID: "acme", ClusterID: "c-1",
			ClusterPrefix: "prod-1a2b3c4d", NodeGroupName: "prod-1a2b3c4d-default"},
		IAM: crossplaneaws.ComponentIAMPlan{Namespace: "tenant-acme", ProviderConfigName: "aws", TenantID: "acme",
			ClusterID: "c-1", ClusterName: "prod", ClusterPrefix: "prod-1a2b3c4d", Region: "us-east-1"},
	}
	plan, skipped := component.PlanPlatform(in)
	if len(skipped) != 0 {
		t.Fatalf("left out: %v", skipped)
	}
	var got []string
	for _, c := range plan.Components {
		got = append(got, c.Name)
	}
	return got
}

func decoded(t *testing.T, defs []*cloudv1alpha1.ComponentDefinition) []component.Definition {
	t.Helper()
	out := make([]component.Definition, 0, len(defs))
	for _, d := range defs {
		def, err := component.DefinitionOf(d)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, def)
	}
	return out
}

// Adding an ordinary component is data only: its definition file and a
// release pin. It passes `controller --check`, is planned after what it
// requires, and its Deployment is sized on the platform pool.
func TestAnOrdinaryComponentIsDataOnly(t *testing.T) {
	shipped, _ := shippedCatalog(t)
	defs := append(slices.Clone(shipped), decodeDefinition(t, metricsAgent))
	rel := withPin(DefaultRelease(), "metrics-agent", "1.0.0")

	if problems := checkedSet(t, defs, rel); len(problems) > 0 {
		t.Fatalf("controller --check:\n%s", strings.Join(problems, "\n"))
	}
	order := planOf(t, defs, rel, true)
	at, kyverno := slices.Index(order, "metrics-agent"), slices.Index(order, "kyverno")
	if at < 0 || at < kyverno {
		t.Errorf("planned %v: metrics-agent must follow kyverno, which it requires", order)
	}
	rows, problems := catalog.SizingTable(decoded(t, defs), 2)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	want := catalog.SizingLine{Name: "metrics-agent", Replicas: 2, Each: catalog.Usage{CPUMilli: 100, MemMiB: 128, Pods: 1}}
	if !slices.Contains(rows, want) {
		t.Errorf("sizing rows %v lack %v", rows, want)
	}
	_, pool := catalog.CheckPlatformSizing(decoded(t, defs), config.DefaultNodeGroups().Platform)
	_, system := catalog.CheckSystemGroupSizing(decoded(t, defs), config.DefaultNodeGroups().System)
	if !strings.Contains(pool, "metrics-agent") || strings.Contains(system, "metrics-agent") {
		t.Errorf("metrics-agent runs on the platform pool:\npool\n%s\nsystem group\n%s", pool, system)
	}
}

// The special components are found by their role, not their name: the
// shipped Karpenter and Capsule files renamed still pass the check, the
// NodeProvisioner still installs first on the system group, and the
// NamespaceTenancy definition is still installed only with NaaS.
func TestTheRolesNotTheNamesDriveTheSpecialComponents(t *testing.T) {
	shipped, _ := shippedCatalog(t)
	nodes, rel := renamed(t, "karpenter.yaml", "karpenter", "nodes", DefaultRelease())
	tenancy, rel := renamed(t, "capsule.yaml", "capsule", "tenancy", rel)
	defs := []*cloudv1alpha1.ComponentDefinition{nodes, tenancy}
	for _, d := range shipped {
		if d.Name != "karpenter" && d.Name != "capsule" {
			defs = append(defs, d)
		}
	}

	if problems := checkedSet(t, defs, rel); len(problems) > 0 {
		t.Fatalf("controller --check:\n%s", strings.Join(problems, "\n"))
	}
	with, without := planOf(t, defs, rel, true), planOf(t, defs, rel, false)
	if with[0] != "nodes" || without[0] != "nodes" {
		t.Errorf("planned %v / %v: the NodeProvisioner installs first", with, without)
	}
	if !slices.Contains(with, "tenancy") || slices.Contains(without, "tenancy") {
		t.Errorf("planned %v with NaaS, %v without: NamespaceTenancy is NaaS only", with, without)
	}
	items := make([]cloudv1alpha1.ComponentDefinition, 0, len(defs))
	for _, d := range defs {
		items = append(items, *d)
	}
	if got := component.RoleNameOf(items, component.RoleNodeProvisioner, "aws"); got != "nodes" {
		t.Errorf("node provisioner = %s, want nodes", got)
	}
	if got := component.RoleNameOf(items, component.RoleNamespaceTenancy, "aws"); got != "tenancy" {
		t.Errorf("namespace tenancy = %s, want tenancy", got)
	}
	_, bootstrap := catalog.CheckBootstrapSizing(decoded(t, defs), config.DefaultNodeGroups().System)
	if !strings.Contains(bootstrap, "karpenter ") {
		t.Errorf("the bootstrap group counts the NodeProvisioner's Deployment:\n%s", bootstrap)
	}
}
