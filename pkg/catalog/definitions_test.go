package catalog_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/config/crd/crdtest"
	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/release"
)

func definitions(t *testing.T) []*cloudv1alpha1.ComponentDefinition {
	t.Helper()
	defs, err := catalog.Definitions()
	if err != nil {
		t.Fatal(err)
	}
	return defs
}

func byName(t *testing.T) map[string]*cloudv1alpha1.ComponentDefinition {
	t.Helper()
	out := map[string]*cloudv1alpha1.ComponentDefinition{}
	for _, d := range definitions(t) {
		out[d.Name] = d
	}
	return out
}

// The shipped catalog is the components the platform offers.
func TestCatalogHasTheOfferedComponents(t *testing.T) {
	var names []string
	for _, d := range definitions(t) {
		names = append(names, d.Name)
	}
	want := []string{"aws-load-balancer-controller", "capsule", "external-secrets", "gpu-operator", "istio", "istio-base",
		"istio-cni",
		"istio-ingress-gateway", "istiod", "karpenter", "kyverno", "kyverno-policies", "ztunnel"}
	if !slices.Equal(names, want) {
		t.Errorf("catalog = %v, want %v", names, want)
	}
}

// Each file is named after its definition.
func TestFilesAreNamedAfterTheirDefinitions(t *testing.T) {
	files, err := catalog.Files()
	if err != nil {
		t.Fatal(err)
	}
	defs := definitions(t)
	if len(files) != len(defs) {
		t.Fatalf("%d files for %d definitions", len(files), len(defs))
	}
	for _, d := range defs {
		if !slices.Contains(files, d.Name+".yaml") {
			t.Errorf("%s is not in %s.yaml", d.Name, d.Name)
		}
	}
}

// Every file is one the API server accepts: schema, unknown fields, CEL.
func TestDefinitionsPassTheCRD(t *testing.T) {
	v, err := crdtest.New("ComponentDefinition")
	if err != nil {
		t.Fatal(err)
	}
	files, err := catalog.Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		raw, err := catalog.Raw(name)
		if err != nil {
			t.Fatal(err)
		}
		errs, err := v.Validate(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(errs) > 0 {
			t.Errorf("%s: %v", name, errs.ToAggregate())
		}
	}
}

// Every definition is Valid as the controller judges it: supported schema,
// known placeholders in platformValues and the IAM policy, a usable IAM
// role, and a values file a tenant can send back as it is.
func TestDefinitionsAreValid(t *testing.T) {
	for _, def := range definitions(t) {
		d, err := component.DefinitionOf(def)
		if err != nil {
			t.Fatal(err)
		}
		if problems := component.CheckDefinition(d); len(problems) > 0 {
			t.Errorf("%s: %v", def.Name, problems)
		}
	}
}

// catalogDefinitions are the shipped definitions as pkg/component reads
// them.
func catalogDefinitions(t *testing.T) []component.Definition {
	t.Helper()
	var out []component.Definition
	for _, def := range definitions(t) {
		d, err := component.DefinitionOf(def)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func definitionNames(defs []component.Definition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

// The catalog is consistent between its definitions: Platform components
// require only Platform ones, the istio bundle's members exist and are
// internal, and nothing requires in a cycle.
func TestCatalogIsConsistent(t *testing.T) {
	if problems := component.CheckCatalog(catalogDefinitions(t)); len(problems) > 0 {
		t.Errorf("%v", problems)
	}
}

// Tenants see and may enable exactly gpu-operator and kyverno-policies;
// everything else is platform managed (Capsule additionally needs NaaS),
// and the Component API refuses
// it.
func TestWhoControlsWhat(t *testing.T) {
	defs := catalogDefinitions(t)
	if got, want := definitionNames(component.TenantCatalog(defs)), []string{"gpu-operator", "kyverno-policies"}; !slices.Equal(got, want) {
		t.Errorf("tenant components = %v, want %v", got, want)
	}
	platform, err := component.PlatformComponents(defs)
	if err != nil {
		t.Fatal(err)
	}
	got := definitionNames(platform)
	want := []string{"karpenter", "aws-load-balancer-controller", "capsule", "external-secrets", "istio-base", "istiod",
		"istio-cni", "istio-ingress-gateway", "kyverno", "ztunnel"}
	if !slices.Equal(got, want) {
		t.Errorf("installed with the cluster, in order: %v, want %v", got, want)
	}
	for _, d := range defs {
		err := component.CheckTenantAccess(d, defs)
		tenant := d.Name == "gpu-operator" || d.Name == "kyverno-policies"
		if tenant != (err == nil) {
			t.Errorf("%s: tenant access = %v", d.Name, err)
		}
	}
}

// The policy engine is installed by the platform; policies remain tenant opt-in.
func TestKyvernoShipsEnabled(t *testing.T) {
	defs := catalogDefinitions(t)
	off := component.Off(defs)
	for _, name := range []string{"kyverno", "kyverno-policies"} {
		if reason := off[name]; reason != "" {
			t.Errorf("%s unexpectedly unavailable: %s", name, reason)
		}
	}
	for _, d := range defs {
		if d.Name == "kyverno" && d.Disabled {
			t.Error("kyverno must ship enabled")
		}
	}
}

// Istio is one bundle of five internal members, installed in this order.
func TestIstioBundle(t *testing.T) {
	defs := catalogDefinitions(t)
	var istio component.Definition
	for _, d := range defs {
		if d.Name == "istio" {
			istio = d
		}
	}
	order, err := component.BundleOrder(istio, defs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"istio-base", "istiod", "istio-cni", "istio-ingress-gateway", "ztunnel"}
	if !slices.Equal(order, want) {
		t.Errorf("bundle order = %v, want %v", order, want)
	}
	if !istio.IsPlatform() || istio.HasInstall {
		t.Errorf("istio = %+v, want a Platform bundle without an install", istio)
	}
}

// Where every definition's pods run (docs/architecture.md#node-layout,
// "Install order"): phase 1 on the fixed system managed node group
// (${system.*}): Karpenter; phase 2 on Karpenter's platform pool (${platform.*}): every other
// Platform and Tenant definition with Deployments or StatefulSets. Each
// sets the nodeSelector and tolerations placeholders at its chart's keys;
// DaemonSets run on every node.
//
// The keys are checked against `helm template` of the pinned chart (see
// pkg/catalog/release.yaml) with these values: every Deployment, StatefulSet,
// Job and CronJob of the chart then carries the selector and toleration,
// except the DaemonSets listed below. A new definition must be listed in
// placedWorkloads or noWorkloads.
var placedWorkloads = map[string][][]any{
	// Paths of the values at which a nodeSelector and tolerations are set.
	"aws-load-balancer-controller": {{}},
	"capsule":                      {{}, {"global", "jobs", "kubectl"}}, // the Deployment, and the CRD and delete hook Jobs
	"istiod":                       {{}},
	"istio-ingress-gateway":        {{}},
	"kyverno":                      {{"global"}},
	"external-secrets":             {{"global"}}, // the controller, webhook and cert controller
	"karpenter":                    {{}},
	// The operator Deployment and its hook Jobs; Node Feature Discovery's
	// master (and the Job that prunes it on delete) and garbage collector are
	// Deployments too. The operands, and NFD's worker, are DaemonSets that
	// stay on GPU nodes (their charts' GPU tolerations).
	"gpu-operator": {{"operator"}, {"node-feature-discovery", "master"}, {"node-feature-discovery", "gc"}},
}

// DaemonSets run on every node: the chart's own tolerations of everything
// (istio-cni, ztunnel; verified with helm template at the pinned version)
// and no nodeSelector of ours, so they reach the system nodes too.
var placedDaemonSets = map[string]bool{"istio-cni": true, "ztunnel": true}

// Definitions whose charts run no pod of their own: CRDs, the mesh's
// cluster-wide resources, the umbrella bundle, policies Kyverno enforces
// (kyverno-policies: ClusterPolicy-like objects only, verified with helm
// template).
var noWorkloads = map[string]bool{"istio": true, "istio-base": true, "kyverno-policies": true}

// onSystemGroup are the phase-1 definitions: Karpenter alone, beside the
// EKS add-ons (no definitions). Everything else with a workload runs on the
// platform pool.
var onSystemGroup = map[string]bool{"karpenter": true}

func TestEveryWorkloadRunsOnItsPhasesPool(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range catalogDefinitions(t) {
		used := component.Placeholders(d.PlatformValues)
		if phase, _ := component.InstallPhaseOf(d); phase == component.PhaseSystemGroup && !onSystemGroup[d.Name] {
			t.Errorf("%s uses ${system.*}: only Karpenter (and the EKS add-ons) run on the system group", d.Name)
		}
		switch {
		case placedWorkloads[d.Name] != nil:
			seen[d.Name] = true
			for _, path := range placedWorkloads[d.Name] {
				at := lookup(d.PlatformValues, path...)
				selector, tolerations := lookup(at, "nodeSelector"), lookup(at, "tolerations")
				pool := "platform"
				if onSystemGroup[d.Name] {
					pool = "system"
				}
				if selector != "${"+pool+".nodeSelector}" || tolerations != "${"+pool+".tolerations}" {
					t.Errorf("%s: nodeSelector %v, tolerations %v at %v: want ${%s.*}", d.Name, selector, tolerations, path, pool)
				}
			}
		case placedDaemonSets[d.Name]:
			seen[d.Name] = true
			if slices.Contains(used, "system.nodeSelector") || slices.Contains(used, "platform.nodeSelector") {
				t.Errorf("%s is a DaemonSet that must run on every node, but selects a pool", d.Name)
			}
		case noWorkloads[d.Name]:
			seen[d.Name] = true
			if phase, _ := component.InstallPhaseOf(d); phase != component.PhaseUnplaced {
				t.Errorf("%s runs no pod but is placed on the %v", d.Name, phase)
			}
		default:
			t.Errorf("%s (%s) is in no placement list: add where its Deployments take a nodeSelector and tolerations to placedWorkloads, or to placedDaemonSets / noWorkloads", d.Name, d.Management)
		}
	}
	for _, list := range []map[string]bool{placedDaemonSets, noWorkloads} {
		for name := range list {
			if !seen[name] {
				t.Errorf("placement list names %s, which is not in the catalog", name)
			}
		}
	}
	for name := range placedWorkloads {
		if !seen[name] {
			t.Errorf("placedWorkloads names %s, which is not in the catalog", name)
		}
	}
}

// The install order of the catalog: phase 1 (Karpenter, sequence 0, then
// what else runs on the system group) needs nothing of phase 2, and every
// other component with a workload either is on the platform pool (so it
// waits for the pool to be Ready) or requires one that is (istio-cni after
// istiod, ztunnel after istio-cni): nothing in phase 2 starts before the pool.
func TestPhaseTwoWaitsForThePlatformPool(t *testing.T) {
	defs := map[string]component.Definition{}
	for _, d := range catalogDefinitions(t) {
		defs[d.Name] = d
	}
	phase := func(name string) component.InstallPhase {
		p, err := component.InstallPhaseOf(defs[name])
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	var behindPool func(name string, seen map[string]bool) bool
	behindPool = func(name string, seen map[string]bool) bool {
		if phase(name) == component.PhasePlatformPool {
			return true
		}
		for _, r := range defs[name].Requires {
			if !seen[r] {
				seen[r] = true
				if behindPool(r, seen) {
					return true
				}
			}
		}
		return false
	}
	for name, d := range defs {
		switch {
		case onSystemGroup[name]:
			if phase(name) != component.PhaseSystemGroup {
				t.Errorf("%s is phase 1 but is on the %v", name, phase(name))
			}
			for _, r := range d.Requires {
				if phase(r) == component.PhasePlatformPool {
					t.Errorf("%s (phase 1) requires %s, which is on the platform pool", name, r)
				}
			}
		case noWorkloads[name]:
		case !behindPool(name, map[string]bool{}):
			t.Errorf("%s runs pods but neither is on the platform pool nor requires a component that is: it would start before the pool", name)
		}
	}
	if karpenter := defs["karpenter"]; karpenter.Sequence != 0 {
		t.Errorf("karpenter sequence = %d, want 0: it installs first", karpenter.Sequence)
	}
}

// Karpenter must run on the system group (where it launches the others'
// nodes) and off its own nodes.
func TestKarpenterRunsOnTheSystemGroupAndOffItsOwnNodes(t *testing.T) {
	karpenter, err := component.DefinitionOf(byName(t)["karpenter"])
	if err != nil {
		t.Fatal(err)
	}
	if lookup(karpenter.PlatformValues, "affinity", "nodeAffinity") == nil {
		t.Error("karpenter lost the affinity that keeps it off its own nodes")
	}
	if got := lookup(karpenter.PlatformValues, "settings", "interruptionQueue"); got != "${karpenter.interruptionQueue}" {
		t.Errorf("karpenter's interruption queue = %v", got)
	}
	if labels := lookup(karpenter.PlatformValues, "karpenter-nodeclass", "nodePool", "labels"); labels != nil {
		t.Errorf("karpenter's NodePool labels its nodes (%v): the platform applies the NodePools, not the chart", labels)
	}
}

// Nothing autoscales the system managed node group: it has one static
// size, and Karpenter is the only scaler. No shipped definition installs
// cluster-autoscaler, by name or chart.
func TestNoClusterAutoscalerInTheCatalog(t *testing.T) {
	for _, d := range definitions(t) {
		chart := ""
		if d.Spec.Install != nil && d.Spec.Install.Helm != nil {
			chart = d.Spec.Install.Helm.Chart
		}
		if strings.Contains(d.Name, "cluster-autoscaler") || chart == "cluster-autoscaler" {
			t.Errorf("definition %s installs cluster-autoscaler (chart %q)", d.Name, chart)
		}
	}
}

// Only gpu-operator and kyverno-policies take tenant values; every other
// component runs as the platform set it.
func TestOnlyTwoDefinitionsAcceptOverrides(t *testing.T) {
	var open []string
	for _, def := range definitions(t) {
		if def.Spec.ValuesSchema != nil || def.Spec.UserValues != "" {
			open = append(open, def.Name)
		}
	}
	if want := []string{"gpu-operator", "kyverno-policies"}; !slices.Equal(open, want) {
		t.Errorf("definitions accepting overrides = %v, want %v", open, want)
	}
	for _, name := range []string{"gpu-operator", "kyverno-policies"} {
		def := byName(t)[name]
		if def.Spec.ValuesSchema == nil || def.Spec.UserValues == "" {
			t.Errorf("%s has no values file or no schema", name)
		}
	}
}

// Karpenter ships no chart for node classes or node pools (the controller
// applies the platform's and the cluster's own directly): no companion chart,
// and nothing of one left in its values.
func TestKarpenterShipsNoNodeClass(t *testing.T) {
	def := byName(t)["karpenter"]
	if n := len(def.Spec.Install.Helm.Companions); n != 0 {
		t.Errorf("karpenter has %d companion charts, want none", n)
	}
	d, err := component.DefinitionOf(def)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.PlatformValues["karpenter-nodeclass"]; ok {
		t.Error("karpenter still carries karpenter-nodeclass values")
	}
}

// No definition splits its chart: each installs the one upstream chart,
// which brings its own CRDs.
func TestNoSeparateCRDCharts(t *testing.T) {
	for _, def := range definitions(t) {
		if def.Spec.Install == nil {
			continue
		}
		if crds := def.Spec.Install.Helm.CRDs; crds != nil {
			t.Errorf("%s installs a separate CRD chart %s %s", def.Name, crds.Repository, crds.Chart)
		}
	}
}

// entries are the catalog as the release checks take it.
func entries(t *testing.T) []release.Definition {
	t.Helper()
	var out []release.Definition
	for _, d := range definitions(t) {
		out = append(out, release.Definition{Name: d.Name, Requires: d.Spec.Requires,
			VersionGroup: d.Spec.VersionGroup, OneMinorAtATime: d.Spec.UpgradeOneMinorAtATime})
	}
	return out
}

// Every requirement is in the catalog, there is no cycle, and the upgrade
// order puts each component after what it requires.
func TestUpgradeOrder(t *testing.T) {
	order, err := release.UpgradeOrder(entries(t))
	if err != nil {
		t.Fatal(err)
	}
	before := func(a, b string) bool { return slices.Index(order, a) < slices.Index(order, b) }
	for _, pair := range [][2]string{
		{"kyverno", "kyverno-policies"},
		{"istio-base", "istiod"}, {"gateway-api-crds", "istiod"}, {"istiod", "istio-cni"}, {"istio-cni", "ztunnel"},
		{"istiod", "istio-ingress-gateway"}, {"aws-load-balancer-controller", "istio-ingress-gateway"},
	} {
		if !before(pair[0], pair[1]) {
			t.Errorf("%s does not come before %s in %v", pair[0], pair[1], order)
		}
	}
}

// The version groups and the one-minor rule are where the research put
// them: the four upstream Istio charts move together, one minor at a time;
// Kyverno and its policies together.
func TestVersionGroups(t *testing.T) {
	groups := map[string][]string{}
	var stepped []string
	for _, e := range entries(t) {
		if e.VersionGroup != "" {
			groups[e.VersionGroup] = append(groups[e.VersionGroup], e.Name)
		}
		if e.OneMinorAtATime {
			stepped = append(stepped, e.Name)
		}
	}
	istio := []string{"istio-base", "istio-cni", "istio-ingress-gateway", "istiod", "ztunnel"}
	if !slices.Equal(groups["istio"], istio) || !slices.Equal(groups["kyverno"], []string{"kyverno", "kyverno-policies"}) || len(groups) != 2 {
		t.Errorf("groups = %v", groups)
	}
	if !slices.Equal(stepped, istio) {
		t.Errorf("one minor at a time = %v, want %v", stepped, istio)
	}
}

// ownRegistry is where the platform's own charts would be published.
// Nothing is: every chart of the catalog comes from its upstream.
const ownRegistry = "ghcr.io/yindia/cluster-vending"

func TestEveryChartIsUpstream(t *testing.T) {
	for _, def := range definitions(t) {
		if def.Spec.Install == nil {
			continue
		}
		helm := def.Spec.Install.Helm
		repos := []string{helm.Repository}
		if helm.CRDs != nil {
			repos = append(repos, helm.CRDs.Repository)
		}
		for _, c := range helm.Companions {
			repos = append(repos, c.Repository)
		}
		for _, repo := range repos {
			if strings.Contains(repo, ownRegistry) {
				t.Errorf("%s pulls a chart from %s", def.Name, repo)
			}
		}
	}
}

// The ingress gateway is internal; an internet-facing one is another
// definition.
func TestIngressGatewayIsInternal(t *testing.T) {
	d, err := component.DefinitionOf(byName(t)["istio-ingress-gateway"])
	if err != nil {
		t.Fatal(err)
	}
	if got := lookup(d.PlatformValues, "service", "annotations", "service.beta.kubernetes.io/aws-load-balancer-scheme"); got != "internal" {
		t.Errorf("scheme = %v, want internal", got)
	}
}

// istiod and ztunnel name the cluster the same way, and istiod trusts the
// ztunnel service account of the ztunnel component's namespace.
func TestIstioAmbientPieces(t *testing.T) {
	defs := byName(t)
	istiod, err := component.DefinitionOf(defs["istiod"])
	if err != nil {
		t.Fatal(err)
	}
	ztunnel, err := component.DefinitionOf(defs["ztunnel"])
	if err != nil {
		t.Fatal(err)
	}
	a := lookup(istiod.PlatformValues, "global", "multiCluster", "clusterName")
	b := lookup(ztunnel.PlatformValues, "multiCluster", "clusterName")
	if a != "${cluster.name}" || a != b {
		t.Errorf("cluster names: istiod %v, ztunnel %v", a, b)
	}
	if got := lookup(istiod.PlatformValues, "trustedZtunnelNamespace"); got != defs["ztunnel"].Spec.Namespace {
		t.Errorf("istiod trusts ztunnel in %v, ztunnel runs in %s", got, defs["ztunnel"].Spec.Namespace)
	}
	for _, name := range []string{"istiod", "istio-cni"} {
		d, err := component.DefinitionOf(defs[name])
		if err != nil {
			t.Fatal(err)
		}
		if d.PlatformValues["profile"] != "ambient" {
			t.Errorf("%s profile = %v, want ambient", name, d.PlatformValues["profile"])
		}
	}
}

// systemNamespaces are never subject to Kyverno, whatever the catalog.
var systemNamespaces = []string{"kube-system", "kube-public", "kube-node-lease", "kyverno"}

// Kyverno skips every namespace the catalog installs into: its webhooks
// (namespaceSelector), its resource filters and the policies chart's
// exclusions. A new catalog component whose namespace is missing fails
// here.
func TestKyvernoExcludesTheCatalogNamespaces(t *testing.T) {
	defs := byName(t)
	need := slices.Clone(systemNamespaces)
	for _, d := range definitions(t) {
		if d.Spec.Namespace != "" && !slices.Contains(need, d.Spec.Namespace) {
			need = append(need, d.Spec.Namespace)
		}
	}
	kyverno, err := component.DefinitionOf(defs["kyverno"])
	if err != nil {
		t.Fatal(err)
	}
	policies, err := component.DefinitionOf(defs["kyverno-policies"])
	if err != nil {
		t.Fatal(err)
	}
	selector := stringList(t, lookup(kyverno.PlatformValues, "config", "webhooks", "namespaceSelector", "matchExpressions", 0, "values"))
	filters := stringList(t, lookup(kyverno.PlatformValues, "config", "resourceFiltersIncludeNamespaces"))
	excluded := stringList(t, lookup(policies.PlatformValues, "vpolExclude", "excludeNamespaces"))
	for _, ns := range need {
		if !slices.Contains(selector, ns) {
			t.Errorf("kyverno webhook namespaceSelector misses %s", ns)
		}
		if !slices.Contains(excluded, ns) {
			t.Errorf("kyverno-policies vpolExclude misses %s", ns)
		}
		// The chart's default resource filters already hold the system ones.
		if !slices.Contains(systemNamespaces, ns) && !slices.Contains(filters, ns) {
			t.Errorf("kyverno resourceFiltersIncludeNamespaces misses %s", ns)
		}
	}
}

// lookup walks maps by key and lists by index.
func lookup(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			l, _ := v.([]any)
			if k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

func stringList(t *testing.T, v any) []string {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %v", v)
	}
	out := make([]string, 0, len(l))
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// The definitions decode with no status: the controller judges them.
func TestDefinitionsShipWithoutStatus(t *testing.T) {
	for _, d := range definitions(t) {
		if len(d.Status.Conditions) > 0 || d.Status.ObservedGeneration != 0 || !d.CreationTimestamp.Equal(&metav1.Time{}) {
			t.Errorf("%s ships with a status or metadata the server sets", d.Name)
		}
	}
}

// Kyverno ships without its four legacy policy CRDs: they keep its Helm
// release record well under the 1 MiB a Secret may hold (about 57% with
// them off, 93% with them on, measured on chart 3.10.0).
func TestKyvernoLeavesOutTheLegacyPolicyCRDs(t *testing.T) {
	d, err := component.DefinitionOf(byName(t)["kyverno"])
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"cleanuppolicies", "clustercleanuppolicies", "clusterpolicies", "policies"} {
		if got := lookup(d.PlatformValues, "crds", "groups", "kyverno", group); got != false {
			t.Errorf("crds.groups.kyverno.%s = %v, want false", group, got)
		}
	}
}

// Every shipped definition is for AWS: a definition is for one cloud, and
// a variant for another cloud is its own definition, <name>-<provider>.
func TestShippedDefinitionsAreForAWS(t *testing.T) {
	for _, d := range definitions(t) {
		if d.Spec.Provider != cloudv1alpha1.CloudProviderAWS {
			t.Errorf("%s is for %q, want aws", d.Name, d.Spec.Provider)
		}
	}
}

// Heavy charts get a longer rollout wait than Helm's ten minutes default:
// operators with hook Jobs and the mesh control plane.
func TestHeavyChartsWaitLonger(t *testing.T) {
	defs := byName(t)
	for _, name := range []string{"gpu-operator", "istiod", "istio-ingress-gateway", "kyverno"} {
		d := defs[name]
		if d == nil || d.Spec.Install == nil || d.Spec.Install.Timeout == nil || d.Spec.Install.Timeout.Duration != 15*time.Minute {
			t.Errorf("%s: install.timeout is not 15m", name)
		}
	}
	for name, d := range defs {
		if d.Spec.Install != nil && d.Spec.Install.Timeout != nil && d.Spec.Install.Timeout.Duration < 10*time.Minute {
			t.Errorf("%s: install.timeout %s is shorter than the default", name, d.Spec.Install.Timeout.Duration)
		}
	}
}
