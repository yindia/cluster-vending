package catalog_test

import (
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/config"
)

// The sizing rule itself is catalog.CheckSystemGroupSizing (the `controller
// --check` rule); this file holds it to the SHIPPED catalog and the DEFAULT
// system group: what the shipped catalog REQUESTS there must fit the default
// group at its static size with headroom (docs/architecture.md#node-layout,
// "Sizing the system group").

func TestAllocatableFollowsTheEKSReservations(t *testing.T) {
	// t3.medium: 2000 - (60 + 10) = 1930m; 4096 x 0.925 = 3788 MiB less
	// 255 + 11 x 17 + 100 = 542 MiB.
	medium, _ := catalog.InstanceShape("t3.medium")
	cpu, mem := medium.Allocatable()
	if cpu != 1930 || mem != 3246 {
		t.Errorf("t3.medium allocatable = %dm / %d MiB, want 1930m / 3246 MiB", cpu, mem)
	}
	// t3.xlarge: 4000 - (60 + 10 + 10) = 3920m.
	xlarge, _ := catalog.InstanceShape("t3.xlarge")
	if cpu, _ := xlarge.Allocatable(); cpu != 3920 {
		t.Errorf("t3.xlarge allocatable CPU = %dm, want 3920m", cpu)
	}
}

func TestSizingNamesEveryPlatformDefinition(t *testing.T) {
	// Every key of the lists is a Platform definition (no stale entries).
	platform := map[string]bool{}
	for _, d := range catalogDefinitions(t) {
		if d.Management == component.Platform {
			platform[d.Name] = true
		}
	}
	deployments, daemonSets, without := catalog.SizedPlatformNames()
	for list, names := range map[string][]string{"platformDeployments": deployments, "platformDaemonSets": daemonSets, "platformWithoutWorkloads": without} {
		for _, name := range names {
			if !platform[name] {
				t.Errorf("%s names %s, which is not a Platform definition", list, name)
			}
		}
	}
}

// The default system group, at its static size, holds the platform's
// requests with headroom (pods limit included).
func TestCatalogFitsTheDefaultSystemNodeGroup(t *testing.T) {
	problems, summary := catalog.CheckSystemGroupSizing(catalogDefinitions(t), config.DefaultNodeGroups().System)
	t.Log("\n" + summary)
	for _, p := range problems {
		t.Errorf("%s\n%s", p, summary)
	}
}

// Capacity is never added by the rule: a smaller default group must fail.
func TestSizingTableIsNotEmpty(t *testing.T) {
	rows, problems := catalog.SizingTable(catalogDefinitions(t), 2)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	if len(rows) < 12 {
		t.Fatalf("sizing table has %d rows, want the whole catalog", len(rows))
	}
	for _, l := range rows {
		if l.Replicas < 1 || l.Each.CPUMilli <= 0 {
			t.Errorf("%s: %d replicas of %dm: every row must request something", l.Name, l.Replicas, l.Each.CPUMilli)
		}
	}
}

// The rule on its own: each way a group fails, and what is not counted.
func TestCheckSystemGroupSizing(t *testing.T) {
	group := func(instanceType string, min int) config.SystemNodeGroup {
		return config.SystemNodeGroup{InstanceTypes: []string{instanceType}, MinSize: min}
	}
	defs := catalogDefinitions(t)
	without := func(name string) []component.Definition {
		var out []component.Definition
		for _, d := range defs {
			if d.Name != name {
				out = append(out, d)
			}
		}
		return out
	}
	tests := []struct {
		name  string
		defs  []component.Definition
		group config.SystemNodeGroup
		want  []string // substrings, one per problem
	}{
		{"the default fits", defs, config.DefaultNodeGroups().System, nil},
		{"a bigger instance fits", defs, group("t3.xlarge", 2), nil},
		// Phase 1 only (Karpenter, the add-ons, the
		// DaemonSets): its CPU fits one t3.small, its memory and pods do not.
		{"one small node does not", defs, group("t3.small", 1), []string{"memory requests", "pods exceed"}},
		{"unknown instance type", defs, group("x9.huge", 2), []string{`"x9.huge" is not an instance type the sizing rule knows`}},
		{"a definition no list names", append(append([]component.Definition{}, defs...),
			component.Definition{Name: "extra", Management: component.Platform, HasInstall: true}), config.DefaultNodeGroups().System,
			[]string{"extra is in no sizing list"}},
		{"a request that is not explicit", func() []component.Definition {
			out := without("kyverno")
			return append(out, component.Definition{Name: "kyverno", Management: component.Platform, HasInstall: true,
				PlatformValues: component.Values{"admissionController": map[string]any{"replicas": 1}}})
		}(), config.DefaultNodeGroups().System, []string{`kyverno: platformValues has no "admissionController.container.resources.requests"`}},
	}
	for _, tt := range tests {
		problems, _ := catalog.CheckSystemGroupSizing(tt.defs, tt.group)
		if len(problems) < len(tt.want) || (len(tt.want) == 0 && len(problems) != 0) {
			t.Errorf("%s: problems = %v, want %v", tt.name, problems, tt.want)
			continue
		}
		for _, w := range tt.want {
			if !strings.Contains(strings.Join(problems, "\n"), w) {
				t.Errorf("%s: problems = %v, want one with %q", tt.name, problems, w)
			}
		}
	}
}

// A disabled component, and what requires it, take nothing from the group.
func TestDisabledComponentsAreNotSized(t *testing.T) {
	defs := catalogDefinitions(t)
	for i := range defs {
		if defs[i].Name == "kyverno" {
			defs[i].Disabled = true
		}
	}
	rows, problems := catalog.SizingTable(defs, 2)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	for _, l := range rows {
		if strings.HasPrefix(l.Name, "kyverno") {
			t.Errorf("%s is counted although kyverno is disabled", l.Name)
		}
	}
}

// Only what runs on the system group is sized against it: a component on
// the platform pool (phase 2) takes nothing from it, however large, and is
// sized against the pool instead.
func TestPlatformPoolComponentsAreNotSizedOnTheSystemGroup(t *testing.T) {
	defs := catalogDefinitions(t)
	for i := range defs {
		if defs[i].Name != "istiod" {
			continue
		}
		v := component.Values{}
		for k, val := range defs[i].PlatformValues {
			v[k] = val
		}
		v["autoscaleMin"] = 40
		v["resources"] = map[string]any{"requests": map[string]any{"cpu": "4", "memory": "1Gi"}}
		defs[i].PlatformValues = v
	}

	problems, summary := catalog.CheckSystemGroupSizing(defs, config.DefaultNodeGroups().System)

	if len(problems) != 0 {
		t.Errorf("a platform-pool component counted against the system group: %v", problems)
	}
	for _, name := range []string{"istiod", "kyverno-admission", "aws-load-balancer-controller", "external-secrets"} {
		if strings.Contains(summary, name) {
			t.Errorf("system group summary lists %s, which runs on the platform pool:\n%s", name, summary)
		}
	}
	if !strings.Contains(summary, "karpenter") {
		t.Errorf("system group summary lacks karpenter:\n%s", summary)
	}
	if problems, _ := catalog.CheckPlatformSizing(defs, config.DefaultNodeGroups().Platform); len(problems) == 0 {
		t.Error("40 istiod replicas of 4 CPU fit the platform pool's default limits: the pool sizing did not count them")
	}
}
