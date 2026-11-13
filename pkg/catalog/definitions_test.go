package catalog_test

import (
	"slices"
	"strings"
	"testing"

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
	want := []string{"aws-load-balancer-controller", "cluster-autoscaler", "gpu-operator", "istio", "istio-base", "istio-cni",
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
// everything else comes with the cluster, and the Component API refuses
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
	want := []string{"aws-load-balancer-controller", "cluster-autoscaler", "istio-base", "istiod",
		"istio-cni", "istio-ingress-gateway", "karpenter", "kyverno", "ztunnel"}
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

// The Deployments of the catalog run on the cluster's default node pool:
// each definition sets the node selector and the tolerations placeholders
// at its chart's keys. DaemonSets (istio-cni, ztunnel, the GPU operands)
// run on every node and keep their charts' tolerations.
func TestPlacementOnTheDefaultNodePool(t *testing.T) {
	where := map[string][]any{
		"aws-load-balancer-controller": {},
		"cluster-autoscaler":           {},
		"karpenter":                    {},
		"istiod":                       {},
		"istio-ingress-gateway":        {},
		"kyverno":                      {"global"},
		"gpu-operator":                 {"operator"},
	}
	for _, d := range catalogDefinitions(t) {
		path, placed := where[d.Name]
		at := lookup(d.PlatformValues, path...)
		selector := lookup(at, "nodeSelector")
		tolerations := lookup(at, "tolerations")
		if placed && (selector != "${system.nodeSelector}" || tolerations != "${system.tolerations}") {
			t.Errorf("%s: nodeSelector %v, tolerations %v at %v", d.Name, selector, tolerations, path)
		}
		if !placed && slices.Contains(component.Placeholders(d.PlatformValues), "system.nodeSelector") {
			t.Errorf("%s selects the default node pool; it is not expected to", d.Name)
		}
	}
	karpenter, err := component.DefinitionOf(byName(t)["karpenter"])
	if err != nil {
		t.Fatal(err)
	}
	if lookup(karpenter.PlatformValues, "affinity", "nodeAffinity") == nil {
		t.Error("karpenter lost the affinity that keeps it off its own nodes")
	}
	if labels := lookup(karpenter.PlatformValues, "karpenter-nodeclass", "nodePool", "labels"); labels != nil {
		t.Errorf("karpenter's NodePool labels its nodes (%v): they must not carry the default pool's labels", labels)
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

// Karpenter ships without a node class or node pool: no companion chart,
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
