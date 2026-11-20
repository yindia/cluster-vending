package catalog_test

import (
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/config"
)

// PROP-03 4.5: the bootstrap set against the bootstrap group (CPU, memory
// and pods per node), and the platform set against the platform pool.

func tinyKarpenter(replicas int) component.Definition {
	return component.Definition{Name: "karpenter", Management: component.Platform, HasInstall: true,
		PlatformValues: component.Values{"replicas": replicas, "controller": map[string]any{
			"resources": map[string]any{"requests": map[string]any{"cpu": "1m", "memory": "1Mi"}}}}}
}

func withRequests(defs []component.Definition, name, cpu, memory string) []component.Definition {
	out := make([]component.Definition, 0, len(defs))
	for _, d := range defs {
		if d.Name == name {
			d.PlatformValues = component.Values{"replicaCount": 1, "resources": map[string]any{"requests": map[string]any{"cpu": cpu, "memory": memory}},
				"webhook":        map[string]any{"replicaCount": 1, "resources": map[string]any{"requests": map[string]any{"cpu": "1m", "memory": "1Mi"}}},
				"certController": map[string]any{"replicaCount": 1, "resources": map[string]any{"requests": map[string]any{"cpu": "1m", "memory": "1Mi"}}}}
		}
		out = append(out, d)
	}
	return out
}

func bootstrapGroup(min int, types ...string) config.SystemNodeGroup {
	return config.SystemNodeGroup{InstanceTypes: types, MinSize: min, MaxSize: min + 4}
}

func TestShippedCatalogFitsTheDefaultBootstrapGroup(t *testing.T) {
	problems, summary := catalog.CheckBootstrapSizing(catalogDefinitions(t), config.DefaultNodeGroups().System)
	t.Log("\n" + summary)
	if len(problems) > 0 {
		t.Errorf("%v\n%s", problems, summary)
	}
	// Only the bootstrap set is counted: karpenter, coredns, the EBS
	// controller and the DaemonSets, never kyverno or istiod.
	for _, name := range []string{"kyverno", "istiod", "capsule", "external-secrets", "aws-load-balancer"} {
		if strings.Contains(summary, name) {
			t.Errorf("summary counts %s, which is not bootstrap:\n%s", name, summary)
		}
	}
	for _, name := range []string{"karpenter", "coredns", "ebs-csi controller", "istio-cni", "ztunnel", "vpc-cni"} {
		if !strings.Contains(summary, name) {
			t.Errorf("summary lacks %s:\n%s", name, summary)
		}
	}
}

func TestBootstrapSizingCountsPodsPerNode(t *testing.T) {
	// 40 tiny Karpenter replicas fit CPU and memory but not the 2 x 17
	// pod slots of two t3.medium nodes.
	defs := []component.Definition{tinyKarpenter(40)}
	problems, _ := catalog.CheckBootstrapSizing(defs, bootstrapGroup(2, "t3.medium"))
	text := strings.Join(problems, "\n")
	if !strings.Contains(text, "pods exceed the 34") || !strings.Contains(text, "2 x 17") {
		t.Errorf("problems = %v, want the pod limit of 34 (2 x 17)", problems)
	}
	if strings.Contains(text, "CPU requests") || strings.Contains(text, "memory requests") {
		t.Errorf("CPU and memory fit, got %v", problems)
	}
	// Six replicas and six DaemonSets per node fit: 18 of 34.
	if problems, _ := catalog.CheckBootstrapSizing([]component.Definition{tinyKarpenter(2)}, bootstrapGroup(2, "t3.medium")); len(problems) != 0 {
		t.Errorf("2 replicas: %v", problems)
	}
}

func TestBootstrapSizingIgnoresThePlatformSet(t *testing.T) {
	defs := withRequests(catalogDefinitions(t), "external-secrets", "64", "512Gi")
	if problems, _ := catalog.CheckBootstrapSizing(defs, config.DefaultNodeGroups().System); len(problems) != 0 {
		t.Errorf("a huge external-secrets must not fail the bootstrap check: %v", problems)
	}
}

func TestBootstrapSizingOfAListUsesTheSmallestType(t *testing.T) {
	group := bootstrapGroup(2, "m5.large", "t3.medium") // same 2 vCPU class, 8 and 4 GiB
	problems, summary := catalog.CheckBootstrapSizing(catalogDefinitions(t), group)
	if len(problems) != 0 {
		t.Fatalf("%v", problems)
	}
	if !strings.Contains(summary, "2 x t3.medium") || !strings.Contains(summary, "34 pods") {
		t.Errorf("summary is not of the smallest type (17 pods a node):\n%s", summary)
	}
}

func TestBootstrapSizingRefusesWhatItCannotSize(t *testing.T) {
	defs := catalogDefinitions(t)
	for name, tt := range map[string]struct {
		group config.SystemNodeGroup
		want  string
	}{
		"unknown type":          {bootstrapGroup(2, "x9.huge"), `"x9.huge" is not an instance type the sizing rule knows`},
		"mixed vCPU classes":    {bootstrapGroup(2, "t3.medium", "m6i.xlarge"), "vCPU"},
		"mixed memory classes":  {bootstrapGroup(2, "t3.medium", "r6i.large"), "not an instance type"},
		"empty list":            {bootstrapGroup(2), "no instance type"},
		"too small and 1 pod":   {bootstrapGroup(1, "t3.small"), "memory requests"},
		"big memory class gaps": {bootstrapGroup(2, "t3.small", "m6i.large"), "memory"},
	} {
		problems, _ := catalog.CheckBootstrapSizing(defs, tt.group)
		if !strings.Contains(strings.Join(problems, "\n"), tt.want) {
			t.Errorf("%s: problems = %v, want %q", name, problems, tt.want)
		}
	}
}

func TestShippedCatalogFitsTheDefaultPlatformPool(t *testing.T) {
	problems, summary := catalog.CheckPlatformSizing(catalogDefinitions(t), config.DefaultNodeGroups().Platform)
	t.Log("\n" + summary)
	if len(problems) > 0 {
		t.Errorf("%v\n%s", problems, summary)
	}
	for _, name := range []string{"kyverno-admission", "istiod", "capsule", "external-secrets", "aws-load-balancer-controller"} {
		if !strings.Contains(summary, name) {
			t.Errorf("summary lacks %s:\n%s", name, summary)
		}
	}
	for _, name := range []string{"karpenter", "cluster-autoscaler", "coredns", "istio-cni", "ztunnel"} {
		if strings.Contains(summary, name) {
			t.Errorf("summary counts %s, which does not run on the pool:\n%s", name, summary)
		}
	}
}

func TestPlatformSizingRules(t *testing.T) {
	defs := catalogDefinitions(t)
	pool := func(change func(*config.PlatformNodePool)) config.PlatformNodePool {
		p := config.DefaultNodeGroups().Platform
		change(&p)
		return p
	}
	for name, tt := range map[string]struct {
		defs []component.Definition
		pool config.PlatformNodePool
		want string
	}{
		"limits cpu too small": {defs, pool(func(p *config.PlatformNodePool) { p.Limits.CPU = 0 }), "limits.cpu"},
		"limits memory small":  {defs, pool(func(p *config.PlatformNodePool) { p.Limits.Memory = "512Mi" }), "limits.memory"},
		"a request above every shape": {withRequests(defs, "external-secrets", "30", "1Gi"),
			pool(func(p *config.PlatformNodePool) {
				p.InstanceFamilies, p.InstanceSizes = []string{"m7i"}, []string{"large", "xlarge"}
			}),
			"can never schedule"},
		"a memory request above every shape": {withRequests(defs, "external-secrets", "10m", "100Gi"),
			pool(func(p *config.PlatformNodePool) { p.InstanceSizes = []string{"large"} }), "can never schedule"},
		"a family with no known size": {defs, pool(func(p *config.PlatformNodePool) { p.InstanceFamilies = []string{"z9"} }), "z9"},
	} {
		problems, _ := catalog.CheckPlatformSizing(tt.defs, tt.pool)
		if !strings.Contains(strings.Join(problems, "\n"), tt.want) {
			t.Errorf("%s: problems = %v, want %q", name, problems, tt.want)
		}
	}
	// A big request fits when a big size is allowed.
	big := withRequests(defs, "external-secrets", "30", "1Gi")
	allowed := pool(func(p *config.PlatformNodePool) { p.InstanceSizes = []string{"8xlarge"}; p.Limits.CPU = 512 })
	if problems, _ := catalog.CheckPlatformSizing(big, allowed); len(problems) != 0 {
		t.Errorf("30 CPU on an 8xlarge: %v", problems)
	}
}

func TestPlatformSizingSkipsDisabledComponents(t *testing.T) {
	defs := catalogDefinitions(t)
	for i := range defs {
		if defs[i].Name == "kyverno" {
			defs[i].Disabled = true
		}
	}
	_, summary := catalog.CheckPlatformSizing(defs, config.DefaultNodeGroups().Platform)
	if strings.Contains(summary, "kyverno") {
		t.Errorf("kyverno is disabled but counted:\n%s", summary)
	}
}

// controller --check runs the platform rule when it has the config: the
// small test catalog has no requests, which the rule reports.
func TestCheckRunsThePlatformRule(t *testing.T) {
	ng := config.DefaultNodeGroups()
	opts := checkOptions
	opts.PlatformPool = &ng.Platform
	text := strings.Join(problemStrings(catalog.Check(goodSet(), opts)), "\n")
	if !strings.Contains(text, "karpenter: platformValues has no") {
		t.Errorf("problems = %s, want the platform rule to have run", text)
	}
	opts.PlatformPool = nil
	if problems := catalog.Check(goodSet(), opts); len(problems) > 0 {
		t.Errorf("without nodeGroups: %v", problems)
	}
}
