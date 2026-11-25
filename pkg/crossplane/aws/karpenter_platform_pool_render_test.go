package aws_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// PROP-03 4.3 and phase 2: the EC2NodeClass and NodePool `platform-pool`.

func defaultPlatformPool() *crossplaneaws.PlatformPoolInput {
	return &crossplaneaws.PlatformPoolInput{
		Families: []string{"m7i", "m6i"}, Sizes: []string{"large", "xlarge", "2xlarge"},
		Architecture: "AMD64", CapacityTypes: []string{"ON_DEMAND"},
		LimitCPU: 128, LimitMemory: "512Gi",
		ConsolidationPolicy: "WhenEmpty", ConsolidateAfter: 5 * time.Minute,
		ExpireAfter: 336 * time.Hour, TerminationGracePeriod: time.Hour, DisruptionBudget: 1,
	}
}

func platformInput(pool *crossplaneaws.PlatformPoolInput) crossplaneaws.KarpenterNodeGroupsInput {
	in := nodeGroupsInput()
	in.PlatformPool = pool
	return in
}

// asJSONMap normalises an object the way the API server sees it.
func asJSONMap(t *testing.T, o *unstructured.Unstructured) map[string]any {
	t.Helper()
	raw, err := json.Marshal(o.Object)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func fromYAML(t *testing.T, doc string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The golden NodePool of the proposal's 4.3, for the default config.
const goldenPlatformNodePool = `
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: platform-pool
  labels:
    cloud.cluster-vending.io/managed-by: platform
spec:
  template:
    metadata:
      labels:
        node.cluster-vending.io/pool: platform-pool
    spec:
      nodeClassRef: {group: karpenter.k8s.aws, kind: EC2NodeClass, name: platform-pool}
      taints:
      - key: node.cluster-vending.io/pool
        value: platform-pool
        effect: NoSchedule
      requirements:
      - {key: kubernetes.io/arch, operator: In, values: [amd64]}
      - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
      - {key: karpenter.k8s.aws/instance-family, operator: In, values: [m6i, m7i]}
      - {key: karpenter.k8s.aws/instance-size, operator: In, values: [2xlarge, large, xlarge]}
      expireAfter: 336h
      terminationGracePeriod: 1h
  limits:
    cpu: "128"
    memory: 512Gi
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 5m
    budgets:
    - nodes: "1"
`

const goldenPlatformNodeClass = `
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: platform-pool
  labels:
    cloud.cluster-vending.io/managed-by: platform
spec:
  role: prod-1a2b3c4d-karpenter-node
  amiSelectorTerms:
  - alias: al2023@v20261107
  subnetSelectorTerms:
  - tags: {karpenter.sh/discovery: prod}
  securityGroupSelectorTerms:
  - tags: {aws:eks:cluster-name: prod}
`

func TestPlatformPoolGolden(t *testing.T) {
	objs, err := crossplaneaws.KarpenterNodeGroupObjects(platformInput(defaultPlatformPool()))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("%d objects, want the class and the pool", len(objs))
	}
	if objs[0].GetKind() != "EC2NodeClass" || objs[1].GetKind() != "NodePool" {
		t.Errorf("order = %s, %s: a class before its pool", objs[0].GetKind(), objs[1].GetKind())
	}
	if got, want := asJSONMap(t, objs[1]), fromYAML(t, goldenPlatformNodePool); !reflect.DeepEqual(got, want) {
		gj, _ := yaml.Marshal(got)
		t.Errorf("NodePool differs from the proposal's:\n%s", gj)
	}
	if got, want := asJSONMap(t, objs[0]), fromYAML(t, goldenPlatformNodeClass); !reflect.DeepEqual(got, want) {
		gj, _ := yaml.Marshal(got)
		t.Errorf("EC2NodeClass differs:\n%s", gj)
	}
}

// The pool is named, labelled and tainted with one value, platform-pool.
func TestPlatformPoolNameLabelAndTaintAreOneValue(t *testing.T) {
	if crossplaneaws.PoolPlatform != "platform-pool" {
		t.Fatalf("PoolPlatform = %q", crossplaneaws.PoolPlatform)
	}
	if got := crossplaneaws.PoolTaint(crossplaneaws.PoolPlatform); got.Key != crossplaneaws.PoolLabelKey || got.Value != "platform-pool" || got.Effect != "NO_SCHEDULE" {
		t.Errorf("taint = %+v", got)
	}
	if got := crossplaneaws.PoolLabels(crossplaneaws.PoolPlatform); got[crossplaneaws.PoolLabelKey] != "platform-pool" {
		t.Errorf("labels = %v", got)
	}
}

// Same input, same bytes: the pool's class, then its pool.
func TestPlatformPoolIsDeterministic(t *testing.T) {
	in := platformInput(defaultPlatformPool())
	first, err := crossplaneaws.KarpenterNodeGroupObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range first {
		names = append(names, o.GetKind()+"/"+o.GetName())
	}
	want := []string{"EC2NodeClass/platform-pool", "NodePool/platform-pool"}
	if !slices.Equal(names, want) {
		t.Errorf("objects = %v, want %v", names, want)
	}
	again, _ := crossplaneaws.KarpenterNodeGroupObjects(in)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(again)
	if !bytes.Equal(a, b) {
		t.Error("the same input gave different objects")
	}
}

func TestPlatformPoolFollowsItsSettings(t *testing.T) {
	pool := defaultPlatformPool()
	pool.Architecture, pool.Families = "ARM64", []string{"m7g"}
	pool.ConsolidationPolicy, pool.ConsolidateAfter = "WhenEmptyOrUnderutilized", 90*time.Second
	pool.DisruptionBudget, pool.MaxPods = 3, 110
	pool.LimitCPU, pool.LimitMemory = 32, "128Gi"
	pool.ExpireAfter, pool.TerminationGracePeriod = 72*time.Hour, 30*time.Minute
	objs, err := crossplaneaws.KarpenterNodeGroupObjects(platformInput(pool))
	if err != nil {
		t.Fatal(err)
	}
	np, class := asJSONMap(t, objs[1]), asJSONMap(t, objs[0])
	spec := np["spec"].(map[string]any)
	if d := spec["disruption"].(map[string]any); d["consolidationPolicy"] != "WhenEmptyOrUnderutilized" || d["consolidateAfter"] != "1m30s" ||
		!reflect.DeepEqual(d["budgets"], []any{map[string]any{"nodes": "3"}}) {
		t.Errorf("disruption = %v", d)
	}
	if !reflect.DeepEqual(spec["limits"], map[string]any{"cpu": "32", "memory": "128Gi"}) {
		t.Errorf("limits = %v", spec["limits"])
	}
	tpl := spec["template"].(map[string]any)["spec"].(map[string]any)
	if tpl["expireAfter"] != "72h" || tpl["terminationGracePeriod"] != "30m" {
		t.Errorf("expire %v grace %v", tpl["expireAfter"], tpl["terminationGracePeriod"])
	}
	if got := reqValues(t, objs[1], "kubernetes.io/arch"); !slices.Equal(got, []string{"arm64"}) {
		t.Errorf("arch = %v", got)
	}
	if got := class["spec"].(map[string]any)["kubelet"]; !reflect.DeepEqual(got, map[string]any{"maxPods": float64(110)}) {
		t.Errorf("kubelet = %v, want maxPods 110", got)
	}
	// maxPods 0 adds no kubelet settings.
	objs, _ = crossplaneaws.KarpenterNodeGroupObjects(platformInput(defaultPlatformPool()))
	if _, set := objs[0].Object["spec"].(map[string]any)["kubelet"]; set {
		t.Error("kubelet set although maxPods is 0")
	}
}

// Spot is refused here too, whatever the input says.
func TestPlatformPoolRefusesSpotAndEmptyShapes(t *testing.T) {
	for name, change := range map[string]func(*crossplaneaws.PlatformPoolInput){
		"spot":         func(p *crossplaneaws.PlatformPoolInput) { p.CapacityTypes = []string{"SPOT"} },
		"no families":  func(p *crossplaneaws.PlatformPoolInput) { p.Families = nil },
		"no sizes":     func(p *crossplaneaws.PlatformPoolInput) { p.Sizes = nil },
		"bad arch":     func(p *crossplaneaws.PlatformPoolInput) { p.Architecture = "x86" },
		"no policy":    func(p *crossplaneaws.PlatformPoolInput) { p.ConsolidationPolicy = "" },
		"zero budget":  func(p *crossplaneaws.PlatformPoolInput) { p.DisruptionBudget = 0 },
		"empty limits": func(p *crossplaneaws.PlatformPoolInput) { p.LimitMemory = "" },
	} {
		pool := defaultPlatformPool()
		change(pool)
		if _, err := crossplaneaws.KarpenterNodeGroupObjects(platformInput(pool)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
