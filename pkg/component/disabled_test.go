package component

import (
	"slices"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/fault"
)

// disable returns the catalog with the named definitions switched off.
func disable(defs []Definition, off ...string) []Definition {
	out := slices.Clone(defs)
	for i := range out {
		if slices.Contains(off, out[i].Name) {
			out[i].Disabled = true
		}
	}
	return out
}

// A disabled Platform definition is not installed, and neither is what
// cannot work without it: a disabled bundle's members, and whatever
// requires something that is off.
func TestPlatformComponentsLeaveOutWhatIsDisabled(t *testing.T) {
	tests := []struct {
		name string
		off  []string
		want []string
		why  map[string]string
	}{
		{name: "nothing disabled", want: []string{"base", "crds", "istiod", "kyverno", "lb", "gateway"}, why: map[string]string{}},
		{name: "a component nothing requires", off: []string{"gpu-operator"}, want: []string{"base", "crds", "istiod", "kyverno", "lb", "gateway"},
			why: map[string]string{"gpu-operator": "disabled"}},
		{name: "a Platform component a Tenant one requires", off: []string{"kyverno"}, want: []string{"base", "crds", "istiod", "lb", "gateway"},
			why: map[string]string{"kyverno": "disabled", "kyverno-policies": "it requires kyverno, which is off"}},
		{name: "a bundle", off: []string{"istio"}, want: []string{"kyverno", "lb"},
			why: map[string]string{"istio": "disabled", "base": "its bundle istio is off", "crds": "its bundle istio is off",
				"istiod": "its bundle istio is off", "gateway": "its bundle istio is off"}},
		{name: "a component another requires", off: []string{"lb"}, want: []string{"base", "crds", "istiod", "kyverno"},
			why: map[string]string{"lb": "disabled", "gateway": "it requires lb, which is off"}},
		{name: "a member of a bundle", off: []string{"base"}, want: []string{"crds", "kyverno", "lb"},
			why: map[string]string{"base": "disabled", "istiod": "it requires base, which is off", "gateway": "it requires istiod, which is off"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog := disable(mesh(), tt.off...)

			got, err := PlatformComponents(catalog)
			off := Off(catalog)

			if err != nil || !slices.Equal(names(got), tt.want) {
				t.Errorf("platform components = %v, %v; want %v", names(got), err, tt.want)
			}
			if len(off) != len(tt.why) {
				t.Errorf("Off = %v, want %v", off, tt.why)
			}
			for name, why := range tt.why {
				if off[name] != why {
					t.Errorf("Off[%s] = %q, want %q", name, off[name], why)
				}
			}
		})
	}
}

// Off does not change the catalog it is given.
func TestOffLeavesItsInputAlone(t *testing.T) {
	catalog := disable(mesh(), "kyverno")
	before := names(catalog)

	Off(catalog)

	if !slices.Equal(names(catalog), before) || !named(catalog, "kyverno").Disabled || named(catalog, "lb").Disabled {
		t.Fatalf("Off changed its input: %v", catalog)
	}
}

// A disabled Tenant definition stays in the catalog, marked, and cannot be
// enabled or changed; what is enabled can still be disabled.
func TestDisabledTenantDefinition(t *testing.T) {
	cluster := &Cluster{Name: "prod", Running: true, KubernetesMinor: "1.37", IRSA: true}
	offering := &Offering{Release: "r", Supported: true, Versions: map[string]string{
		"kyverno": "3.10.0", "kyverno-policies": "3.10.0", "gpu-operator": "v26.7.2"}}
	tests := []struct {
		name      string
		off       []string
		component string
		update    bool
		want      string
	}{
		{"enable a disabled component", []string{"gpu-operator"}, "gpu-operator", false, "component gpu-operator is disabled by the platform"},
		{"update a disabled component", []string{"gpu-operator"}, "gpu-operator", true, "component gpu-operator is disabled by the platform"},
		{"enable what needs a disabled Platform component", []string{"kyverno"}, "kyverno-policies", false,
			"component kyverno-policies is off: it requires kyverno, which is off"},
		{"enable a component that is on", []string{"kyverno"}, "gpu-operator", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog := disable(mesh(), tt.off...)
			d := named(catalog, tt.component)
			r := Request{Component: tt.component, Definition: &d, Cluster: cluster, Offering: offering, Catalog: catalog, Update: tt.update}
			if tt.update {
				r.Enabled = []string{tt.component}
			}

			problems := CheckEnable(r)
			err := Err(problems)

			if tt.want == "" {
				if err != nil {
					t.Fatalf("CheckEnable = %v", err)
				}
				return
			}
			if len(problems) != 1 || !fault.Is(err, fault.FailedPrecondition) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("CheckEnable = %v, want one FailedPrecondition naming %q", err, tt.want)
			}
		})
	}
	catalog := disable(mesh(), "gpu-operator")
	if got, want := names(TenantCatalog(catalog)), []string{"gpu-operator", "kyverno-policies"}; !slices.Equal(got, want) {
		t.Errorf("tenant catalog = %v, want %v (a disabled one is listed)", got, want)
	}
	if !named(TenantCatalog(catalog), "gpu-operator").Disabled {
		t.Error("the catalog entry is not marked disabled")
	}
	if err := CheckDisable("gpu-operator", catalog); err != nil {
		t.Errorf("disable an enabled component whose definition is disabled: %v", err)
	}
}
