package release_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/release"
)

var istio = []release.Definition{
	{Name: "istio-base", VersionGroup: "istio", OneMinorAtATime: true},
	{Name: "istiod", Requires: []string{"istio-base"}, VersionGroup: "istio", OneMinorAtATime: true},
	{Name: "istio-ingressgateway", Requires: []string{"istiod", "aws-load-balancer-controller"}, VersionGroup: "istio", OneMinorAtATime: true},
	{Name: "aws-load-balancer-controller"},
	{Name: "kyverno", VersionGroup: "kyverno"},
	{Name: "kyverno-policies", Requires: []string{"kyverno"}, VersionGroup: "kyverno"},
}

func pins(minor string, nameVersion ...string) release.Minor {
	m := release.Minor{Minor: minor}
	for i := 0; i+1 < len(nameVersion); i += 2 {
		m.Components = append(m.Components, release.Component{Name: nameVersion[i], Version: nameVersion[i+1]})
	}
	return m
}

func pinned(minors ...release.Minor) release.Release {
	return release.Release{Name: "r", Kubernetes: minors}
}

func TestCheckGroups(t *testing.T) {
	tests := []struct {
		name string
		r    release.Release
		want []string // substrings, one per problem
	}{
		{"one version per group", pinned(pins("1.37", "istio-base", "1.32.0", "istiod", "1.32.0", "kyverno", "3.10.0", "kyverno-policies", "3.10.0")), nil},
		{"a group partly pinned", pinned(pins("1.37", "istiod", "1.32.0")), nil},
		{"components outside groups differ freely", pinned(pins("1.37", "aws-load-balancer-controller", "3.6.2", "unknown", "1.0")), nil},
		{"istio split", pinned(pins("1.37", "istio-base", "1.32.0", "istiod", "1.31.4", "istio-ingressgateway", "1.32.0")),
			[]string{"on 1.37, version group istio is pinned at more than one version: istiod at 1.31.4; istio-base, istio-ingressgateway at 1.32.0"}},
		{"two minors, two groups", pinned(
			pins("1.36", "kyverno", "3.9.0", "kyverno-policies", "3.10.0"),
			pins("1.37", "istio-base", "1.32.0", "istiod", "1.31.0")),
			[]string{"on 1.36, version group kyverno", "on 1.37, version group istio"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := release.CheckGroups(tt.r, istio)
			if len(got) != len(tt.want) {
				t.Fatalf("got %q, want %d problems", got, len(tt.want))
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("problem %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

func TestCheckStep(t *testing.T) {
	tests := []struct {
		name          string
		current, next release.Release
		want          []string
	}{
		{"one minor up", pinned(pins("1.37", "istiod", "1.31.4")), pinned(pins("1.37", "istiod", "1.32.0")), nil},
		{"patch only", pinned(pins("1.37", "istiod", "1.32.0")), pinned(pins("1.37", "istiod", "1.32.1")), nil},
		{"same version", pinned(pins("1.37", "istiod", "1.32.0")), pinned(pins("1.37", "istiod", "1.32.0")), nil},
		{"down is not a skip", pinned(pins("1.37", "istiod", "1.32.0")), pinned(pins("1.37", "istiod", "1.31.0")), nil},
		{"skips a minor", pinned(pins("1.37", "istiod", "1.30.8")), pinned(pins("1.37", "istiod", "1.32.0")),
			[]string{"on 1.37, istiod moves from 1.30.8 to 1.32.0, skipping 1 minor version(s)"}},
		{"major change", pinned(pins("1.37", "istiod", "1.32.0")), pinned(pins("1.37", "istiod", "2.0.0")),
			[]string{"a change of major version"}},
		{"a component free to jump", pinned(pins("1.37", "kyverno", "3.8.0")), pinned(pins("1.37", "kyverno", "3.10.0")), nil},
		{"new in next", pinned(pins("1.37")), pinned(pins("1.37", "istiod", "1.32.0")), nil},
		{"new Kubernetes minor compares with the highest current pin",
			pinned(pins("1.36", "istiod", "1.30.0"), pins("1.37", "istiod", "1.31.0")),
			pinned(pins("1.38", "istiod", "1.33.0")),
			[]string{"on 1.38, istiod moves from 1.31.0 to 1.33.0"}},
		{"leading v", pinned(pins("1.37", "istiod", "v1.30.0")), pinned(pins("1.37", "istiod", "v1.31.0")), nil},
		{"unparsable versions are not checked", pinned(pins("1.37", "istiod", "latest")), pinned(pins("1.37", "istiod", "1.32.0")), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := release.CheckStep(tt.current, tt.next, istio)
			if len(got) != len(tt.want) {
				t.Fatalf("got %q, want %d problems", got, len(tt.want))
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("problem %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

func TestUpgradeOrder(t *testing.T) {
	got, err := release.UpgradeOrder(istio)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aws-load-balancer-controller", "istio-base", "istiod", "istio-ingressgateway", "kyverno", "kyverno-policies"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	before := func(a, b string) bool { return slices.Index(got, a) < slices.Index(got, b) }
	for _, d := range istio {
		for _, r := range d.Requires {
			if !before(r, d.Name) {
				t.Errorf("%s comes before %s, which it requires", d.Name, r)
			}
		}
	}
}

func TestUpgradeOrderRefuses(t *testing.T) {
	tests := []struct {
		name string
		defs []release.Definition
		want string
	}{
		{"cycle", []release.Definition{{Name: "a", Requires: []string{"b"}}, {Name: "b", Requires: []string{"a"}}, {Name: "c"}},
			"cycle: a, b"},
		{"missing requirement", []release.Definition{{Name: "a", Requires: []string{"x"}}}, "requires x"},
		{"duplicate", []release.Definition{{Name: "a"}, {Name: "a"}}, "twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := release.UpgradeOrder(tt.defs)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// ambient is Istio in ambient mode: six chained components, five in one
// version group that upgrades one minor at a time, and the Gateway API
// CRDs on their own version.
var ambient = []release.Definition{
	{Name: "gateway-api-crds"},
	{Name: "istio-base", Requires: []string{"gateway-api-crds"}, VersionGroup: "istio", OneMinorAtATime: true},
	{Name: "istiod", Requires: []string{"istio-base"}, VersionGroup: "istio", OneMinorAtATime: true},
	{Name: "istio-cni", Requires: []string{"istiod"}, VersionGroup: "istio", OneMinorAtATime: true},
	{Name: "ztunnel", Requires: []string{"istio-cni"}, VersionGroup: "istio", OneMinorAtATime: true},
	{Name: "istio-ingress", Requires: []string{"ztunnel", "istiod"}, VersionGroup: "istio", OneMinorAtATime: true},
}

func ambientPins(minor, istio string) release.Minor {
	return pins(minor, "gateway-api-crds", "1.4.0", "istio-base", istio, "istiod", istio,
		"istio-cni", istio, "ztunnel", istio, "istio-ingress", istio)
}

// A chain of six: the order follows requires, the group holds across all
// five members, and a skipped minor is reported for each of them.
func TestAmbientChain(t *testing.T) {
	order, err := release.UpgradeOrder(ambient)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gateway-api-crds", "istio-base", "istiod", "istio-cni", "ztunnel", "istio-ingress"}
	if !slices.Equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}

	if got := release.CheckGroups(pinned(ambientPins("1.36", "1.32.0"), ambientPins("1.37", "1.32.0")), ambient); len(got) != 0 {
		t.Errorf("one version for the group: %q", got)
	}
	split := ambientPins("1.37", "1.32.0")
	split.Components[4].Version = "1.31.4" // ztunnel
	if got := release.CheckGroups(pinned(split), ambient); len(got) != 1 || !strings.Contains(got[0], "ztunnel at 1.31.4") {
		t.Errorf("ztunnel split from the group: %q", got)
	}

	if got := release.CheckStep(pinned(ambientPins("1.37", "1.31.4")), pinned(ambientPins("1.37", "1.32.0")), ambient); len(got) != 0 {
		t.Errorf("one minor up: %q", got)
	}
	got := release.CheckStep(pinned(ambientPins("1.37", "1.30.8")), pinned(ambientPins("1.37", "1.32.0")), ambient)
	if len(got) != 5 {
		t.Fatalf("a skipped minor is reported for each of the five Istio components, got %q", got)
	}
	for _, p := range got {
		if strings.Contains(p, "gateway-api-crds") {
			t.Errorf("the Gateway API CRDs may move freely: %q", p)
		}
	}
}

// A release pins every member of a bundle on a minor, or none.
func TestCheckBundles(t *testing.T) {
	defs := append(slices.Clone(ambient), release.Definition{Name: "istio",
		Includes: []string{"gateway-api-crds", "istio-base", "istiod", "istio-cni", "ztunnel", "istio-ingress"}})
	if got := release.CheckBundles(pinned(ambientPins("1.37", "1.32.0"), pins("1.36", "kyverno", "3.10.0")), defs); len(got) != 0 {
		t.Errorf("all members on 1.37, none on 1.36: %q", got)
	}
	part := pinned(pins("1.37", "istio-base", "1.32.0", "istiod", "1.32.0"))
	got := release.CheckBundles(part, defs)
	if len(got) != 1 || !strings.Contains(got[0], "on 1.37, istio is pinned in part: gateway-api-crds, istio-cni, istio-ingress, ztunnel missing") {
		t.Errorf("a bundle pinned in part: %q", got)
	}
}

func TestUpgradeOrderEmpty(t *testing.T) {
	got, err := release.UpgradeOrder(nil)
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}
