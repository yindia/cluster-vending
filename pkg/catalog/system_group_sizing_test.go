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
// group at its minimum size with headroom (docs/architecture.md#node-layout,
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

// The default system group, at its MINIMUM size, holds the platform's
// requests with headroom (pods limit included).
func TestCatalogFitsTheDefaultSystemNodeGroup(t *testing.T) {
	problems, summary := catalog.CheckSystemGroupSizing(catalogDefinitions(t), config.DefaultNodeGroups().System)
	t.Log("\n" + summary)
	for _, p := range problems {
		t.Errorf("%s\n%s", p, summary)
	}
}

// The autoscaler has room to add nodes by default, and what it may add is
// at least what the DaemonSets of those nodes take: the maximum is above the
// minimum, and a full group of MaxSize nodes still has room for pods.
func TestDefaultSystemNodeGroupCanGrow(t *testing.T) {
	group := config.DefaultNodeGroups().System
	if group.MaxSize <= group.MinSize {
		t.Fatalf("max %d is not above min %d: cluster-autoscaler could never add a node", group.MaxSize, group.MinSize)
	}
	shape, _ := catalog.InstanceShape(group.InstanceTypes[0])
	nodeCPU, nodeMem := shape.Allocatable()
	maxNodes := int64(group.MaxSize)
	rows, problems := catalog.SizingTable(catalogDefinitions(t), maxNodes)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	var perNodeCPU, perNodeMem int64
	var requested catalog.Usage
	for _, l := range rows {
		requested = requested.Plus(l.Total())
	}
	for _, ds := range catalog.AddonDaemonSetsPerNode() {
		perNodeCPU += ds.Each.CPUMilli
		perNodeMem += ds.Each.MemMiB
	}
	if requested.Pods >= maxNodes*shape.MaxPods || requested.CPUMilli >= nodeCPU*maxNodes || requested.MemMiB >= nodeMem*maxNodes {
		t.Errorf("at the maximum of %d nodes the requests (%dm, %d MiB, %d pods) fill the group (%dm per node of DaemonSets %dm, %d MiB)",
			maxNodes, requested.CPUMilli, requested.MemMiB, requested.Pods, nodeCPU, perNodeCPU, perNodeMem)
	}
	// Each added node nets room: its DaemonSets leave allocatable CPU and memory.
	if nodeCPU-perNodeCPU <= 0 || nodeMem-perNodeMem <= 0 {
		t.Error("a node's add-on DaemonSets take all of it: adding nodes would not help")
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
		return config.SystemNodeGroup{InstanceTypes: []string{instanceType}, MinSize: min, MaxSize: min + 4}
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
		{"one small node does not", defs, group("t3.small", 1), []string{"CPU requests", "memory requests", "pods exceed"}},
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
