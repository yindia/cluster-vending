package component

import (
	"slices"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/fault"
)

// mesh is a small catalog: two Platform components, a Platform bundle of
// three internal members, and two Tenant components.
func mesh() []Definition {
	member := func(name string, requires ...string) Definition {
		return Definition{Name: name, Management: Platform, Internal: true, HasInstall: true, Requires: requires, Valid: true}
	}
	return []Definition{
		{Name: "lb", Management: Platform, HasInstall: true, Valid: true},
		{Name: "kyverno", Management: Platform, HasInstall: true, Valid: true},
		{Name: "istio", Management: Platform, Includes: []string{"crds", "base", "istiod", "gateway"}, Valid: true},
		member("crds"), member("base"), member("istiod", "base", "crds"), member("gateway", "istiod", "lb"),
		{Name: "kyverno-policies", HasInstall: true, Requires: []string{"kyverno"}, Valid: true},
		{Name: "gpu-operator", HasInstall: true, Valid: true},
	}
}

func named(defs []Definition, name string) Definition {
	for _, d := range defs {
		if d.Name == name {
			return d
		}
	}
	return Definition{}
}

func names(defs []Definition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func TestCheckTenantAccess(t *testing.T) {
	catalog := mesh()
	tests := []struct {
		name, component, want string
	}{
		{"tenant component", "gpu-operator", ""},
		{"platform component", "kyverno", "installed with the cluster"},
		{"platform bundle", "istio", "installed with the cluster"},
		{"internal, not platform", "addon", "part of suite; enable, change or disable suite instead"},
		{"internal without a bundle", "orphan", "internal to the platform"},
	}
	catalog = append(catalog,
		Definition{Name: "suite", Includes: []string{"addon"}},
		Definition{Name: "addon", Internal: true, HasInstall: true},
		Definition{Name: "orphan", Internal: true, HasInstall: true})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckTenantAccess(named(catalog, tt.component), catalog)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !fault.Is(err, fault.PermissionDenied) || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), tt.component) {
				t.Errorf("err = %v, want PermissionDenied naming %s with %q", err, tt.component, tt.want)
			}
		})
	}
}

// The Component API refuses Platform and internal names on enable, update
// and disable; a Tenant component that requires a Platform one needs
// nothing enabled first.
func TestRequestsRespectManagement(t *testing.T) {
	catalog := mesh()
	cluster := &Cluster{Name: "prod", Running: true, KubernetesMinor: "1.37", IRSA: true}
	offering := &Offering{Release: "r", Supported: true, Versions: map[string]string{
		"kyverno": "3.10.0", "kyverno-policies": "3.10.0", "istiod": "1.32.0"}}
	request := func(component string, update bool) Request {
		d := named(catalog, component)
		return Request{Component: component, Definition: &d, Cluster: cluster, Offering: offering, Catalog: catalog, Update: update}
	}
	for _, update := range []bool{false, true} {
		for _, component := range []string{"kyverno", "istiod"} {
			problems := CheckEnable(request(component, update))
			if len(problems) != 1 || problems[0].Kind != fault.PermissionDenied {
				t.Errorf("%s (update %v): %+v, want one PermissionDenied", component, update, problems)
			}
			if err := Err(problems); !fault.Is(err, fault.PermissionDenied) {
				t.Errorf("%s: Err = %v", component, err)
			}
		}
	}
	if problems := CheckEnable(request("kyverno-policies", false)); len(problems) != 0 {
		t.Errorf("kyverno-policies needs kyverno, which the platform installs: %+v", problems)
	}
	without := request("kyverno-policies", false)
	without.Catalog = nil
	if problems := CheckEnable(without); len(problems) != 1 || problems[0].Check != 7 {
		t.Errorf("without the catalog the requirement is not known to be the platform's: %+v", problems)
	}
	if err := CheckDisable("kyverno", catalog); !fault.Is(err, fault.PermissionDenied) {
		t.Errorf("disable a Platform component: %v", err)
	}
	if err := CheckDisable("gpu-operator", catalog); err != nil {
		t.Errorf("disable a Tenant component: %v", err)
	}
}

func TestCheckDefinitionManagementAndBundles(t *testing.T) {
	tests := []struct {
		name string
		d    Definition
		want string
	}{
		{"platform, fixed", Definition{Name: "a", Management: Platform, HasInstall: true, PlatformValues: Values{"x": "y"}}, ""},
		{"platform with a values file", Definition{Name: "a", Management: Platform, HasInstall: true, UserValues: "x: 1\n",
			ValuesSchema: Values{"type": "object"}}, "takes no tenant values"},
		{"platform with a schema only", Definition{Name: "a", Management: Platform, HasInstall: true, ValuesSchema: Values{"type": "object"}},
			"takes no tenant values"},
		{"bundle", Definition{Name: "b", Includes: []string{"m"}}, ""},
		{"bundle with an install", Definition{Name: "b", Includes: []string{"m"}, HasInstall: true}, "no install of its own"},
		{"internal bundle", Definition{Name: "b", Includes: []string{"m"}, Internal: true}, "bundles do not nest"},
		{"bundle with values", Definition{Name: "b", Includes: []string{"m"}, PlatformValues: Values{"x": "y"}}, "belong to its members"},
		{"bundle with a role", Definition{Name: "b", Includes: []string{"m"}, CloudResources: role(nil)}, "belong to its members"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.d
			d.Provider = "aws" // TestCheckDefinitionNeedsAProvider covers it
			got := strings.Join(CheckDefinition(d), "; ")
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Errorf("problems = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckCatalog(t *testing.T) {
	if problems := CheckCatalog(mesh()); len(problems) != 0 {
		t.Fatalf("a consistent catalog: %v", problems)
	}
	change := func(f func(defs []Definition) []Definition) []Definition { return f(mesh()) }
	set := func(name string, f func(*Definition)) func([]Definition) []Definition {
		return func(defs []Definition) []Definition {
			for i := range defs {
				if defs[i].Name == name {
					f(&defs[i])
				}
			}
			return defs
		}
	}
	tests := []struct {
		name string
		defs []Definition
		want string
	}{
		{"platform requires a tenant component", change(set("kyverno", func(d *Definition) { d.Requires = []string{"gpu-operator"} })),
			"kyverno is a Platform component but requires gpu-operator"},
		{"unknown member", change(set("istio", func(d *Definition) { d.Includes = append(d.Includes, "ghost") })),
			"istio includes ghost, which the catalog does not have"},
		{"member that is not internal", change(set("base", func(d *Definition) { d.Internal = false })),
			"istio includes base, which is not internal"},
		{"nested bundle", change(func(defs []Definition) []Definition {
			return append(defs, Definition{Name: "outer", Management: Platform, Includes: []string{"istio"}})
		}), "outer includes istio, which is a bundle"},
		{"member of another management", change(set("base", func(d *Definition) { d.Management = Tenant })),
			"istio and its member base differ in management"},
		{"internal without a bundle", change(func(defs []Definition) []Definition {
			return append(defs, Definition{Name: "orphan", Internal: true, HasInstall: true})
		}), "orphan is internal but no bundle includes it"},
		{"member of two bundles", change(func(defs []Definition) []Definition {
			return append(defs, Definition{Name: "other", Management: Platform, Includes: []string{"base"}})
		}), "base is a member of more than one bundle: istio, other"},
		{"tenant member requires a tenant component outside", []Definition{
			{Name: "suite", Includes: []string{"part"}}, {Name: "part", Internal: true, HasInstall: true, Requires: []string{"gpu"}},
			{Name: "gpu", HasInstall: true}}, "part requires gpu, which a tenant controls and is not a member of suite"},
		{"cycle", change(set("base", func(d *Definition) { d.Requires = []string{"istiod"} })), "cycle"},
		{"unknown requirement", change(set("lb", func(d *Definition) { d.Requires = []string{"ghost"} })), "requires ghost"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(CheckCatalog(tt.defs), "; ")
			if !strings.Contains(got, tt.want) {
				t.Errorf("problems = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBundleOrder(t *testing.T) {
	catalog := mesh()
	got, err := BundleOrder(named(catalog, "istio"), catalog)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"base", "crds", "istiod", "gateway"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	tests := []struct {
		name   string
		bundle Definition
		defs   []Definition
		want   string
	}{
		{"not a bundle", named(catalog, "lb"), catalog, "is not a bundle"},
		{"unknown member", Definition{Name: "b", Includes: []string{"ghost"}}, catalog, "does not have"},
		{"member not internal", Definition{Name: "b", Includes: []string{"lb"}}, catalog, "is not internal"},
		{"nested", Definition{Name: "b", Includes: []string{"istio"}}, catalog, "bundles do not nest"},
		{"cycle", Definition{Name: "b", Includes: []string{"x", "y"}}, []Definition{
			{Name: "x", Internal: true, Requires: []string{"y"}}, {Name: "y", Internal: true, Requires: []string{"x"}}}, "cycle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BundleOrder(tt.bundle, tt.defs)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// What the platform installs with a cluster: Platform definitions with a
// chart, bundles expanded to their members, in requires order.
func TestPlatformComponents(t *testing.T) {
	got, err := PlatformComponents(mesh())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"base", "crds", "istiod", "kyverno", "lb", "gateway"}; !slices.Equal(names(got), want) {
		t.Errorf("platform components = %v, want %v", names(got), want)
	}
	bad := mesh()
	bad[1].Requires = []string{"gpu-operator"}
	if _, err := PlatformComponents(bad); err == nil || !strings.Contains(err.Error(), "a tenant controls") {
		t.Errorf("a Platform component requiring a Tenant one: %v", err)
	}
	if none, err := PlatformComponents(nil); err != nil || len(none) != 0 {
		t.Errorf("an empty catalog: %v, %v", none, err)
	}
}

func TestTenantCatalog(t *testing.T) {
	if got, want := names(TenantCatalog(mesh())), []string{"gpu-operator", "kyverno-policies"}; !slices.Equal(got, want) {
		t.Errorf("tenant catalog = %v, want %v", got, want)
	}
}

// Components install in ascending sequence, requires still holding.
func TestPlatformComponentsOrderBySequence(t *testing.T) {
	defs := []Definition{
		{Name: "aaa", Management: Platform, HasInstall: true, Sequence: 100},
		{Name: "zzz", Management: Platform, HasInstall: true, Sequence: 0},
		{Name: "mmm", Management: Platform, HasInstall: true, Sequence: 100, Requires: []string{"aaa"}},
	}

	got, err := PlatformComponents(defs)

	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"zzz", "aaa", "mmm"}; !slices.Equal(names(got), want) {
		t.Errorf("order %v, want %v", names(got), want)
	}
}

// A component cannot require one that installs after it.
func TestPlatformComponentsRejectsRequiringAHigherSequence(t *testing.T) {
	defs := []Definition{
		{Name: "early", Management: Platform, HasInstall: true, Sequence: 0, Requires: []string{"late"}},
		{Name: "late", Management: Platform, HasInstall: true, Sequence: 100},
	}

	if _, err := PlatformComponents(defs); err == nil || !strings.Contains(err.Error(), "sequence") {
		t.Errorf("err = %v", err)
	}
}
