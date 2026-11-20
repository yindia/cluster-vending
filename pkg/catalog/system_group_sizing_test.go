package catalog_test

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/config"
)

// The system managed node group runs Karpenter, cluster-autoscaler, the EKS
// add-on controllers, every platform component and the DaemonSets of each
// node. This file fails when what the shipped catalog REQUESTS there no
// longer fits the DEFAULT group at its MINIMUM size (config.DefaultNodeGroups,
// MinSize nodes) with headroom: a chart bump or a definition edit cannot
// silently outgrow it (docs/architecture.md#node-layout, "Sizing the system group"). The
// group is autoscaled: when pending pods need room, cluster-autoscaler adds
// nodes up to MaxSize, so what does not fit at the minimum is a cost
// (more nodes), not an outage, but the default stays one that needs none.
//
// Only what a definition's platformValues set is counted, so a component's
// requests and replicas must be explicit there: a chart default is never
// guessed (a missing key fails the test). Key names were checked against the
// pinned chart's values with `helm show values` / `helm template` (see
// pkg/catalog/release.yaml for the versions).

// systemGroupFillLimit is the share of the group's allocatable CPU and
// memory the platform may request; the rest is for tenant components,
// rollouts (surge pods) and growth.
const systemGroupFillLimit = 0.85

// instanceShape is an instance type's size and what EKS lets pods use.
type instanceShape struct {
	vCPU    int64 // cores
	memMiB  int64 // instance memory, MiB
	maxPods int64 // EKS max pods with the VPC CNI defaults (ENIs x (IPs - 1) + 2)
}

// instanceShapes are the types an operator may reasonably pick for the
// system group.
var instanceShapes = map[string]instanceShape{
	"t3.small":  {vCPU: 2, memMiB: 2048, maxPods: 11},
	"t3.medium": {vCPU: 2, memMiB: 4096, maxPods: 17},
	"t3.large":  {vCPU: 2, memMiB: 8192, maxPods: 35},
	"t3.xlarge": {vCPU: 4, memMiB: 16384, maxPods: 58},
}

// vmMemoryOverheadPercent is the memory the OS and hypervisor take before
// the kubelet sees it (Karpenter's default assumption, 7.5%).
const vmMemoryOverheadPercent = 0.075

// evictionHardMiB is the kubelet's memory.available eviction threshold.
const evictionHardMiB = 100

// allocatable is what pods may request on one node of the shape, by the EKS
// AL2023 (nodeadm) reservations:
//
//	kube-reserved memory = 255 MiB + 11 MiB x maxPods
//	kube-reserved CPU    = 6% of the 1st core, 1% of the 2nd, 0.5% of the
//	                       next 2, 0.25% of the rest
//	system eviction      = 100 MiB (memory.available)
//
// Memory capacity is the instance memory less vmMemoryOverheadPercent.
func (s instanceShape) allocatable() (cpuMilli, memMiB int64) {
	reserved := int64(60) // 6% of the 1st core
	if s.vCPU > 1 {
		reserved += 10 // 1% of the 2nd
	}
	if s.vCPU > 2 {
		reserved += int64(float64(min(s.vCPU-2, 2)) * 1000 * 0.005)
	}
	if s.vCPU > 4 {
		reserved += int64(float64(s.vCPU-4) * 1000 * 0.0025)
	}
	capacity := int64(float64(s.memMiB) * (1 - vmMemoryOverheadPercent))
	memReserved := 255 + 11*s.maxPods + evictionHardMiB
	return s.vCPU*1000 - reserved, capacity - memReserved
}

func TestAllocatableFollowsTheEKSReservations(t *testing.T) {
	// t3.medium: 2000 - (60 + 10) = 1930m; 4096 x 0.925 = 3788 MiB less
	// 255 + 11 x 17 + 100 = 542 MiB.
	cpu, mem := instanceShapes["t3.medium"].allocatable()
	if cpu != 1930 || mem != 3246 {
		t.Errorf("t3.medium allocatable = %dm / %d MiB, want 1930m / 3246 MiB", cpu, mem)
	}
	// t3.xlarge: 4000 - (60 + 10 + 10) = 3920m.
	if cpu, _ := instanceShapes["t3.xlarge"].allocatable(); cpu != 3920 {
		t.Errorf("t3.xlarge allocatable CPU = %dm, want 3920m", cpu)
	}
}

// usage is a CPU (millicores) and memory (MiB) request and a pod count.
type usage struct {
	cpuMilli, memMiB, pods int64
}

func (u usage) times(n int64) usage { return usage{u.cpuMilli * n, u.memMiB * n, u.pods * n} }

func (u usage) plus(o usage) usage {
	return usage{u.cpuMilli + o.cpuMilli, u.memMiB + o.memMiB, u.pods + o.pods}
}

// line is one row of the sizing table.
type line struct {
	name     string
	replicas int64
	each     usage // one pod
}

func (l line) total() usage { return l.each.times(l.replicas) }

// A Deployment of a Platform definition: where its replicas and per-pod
// requests are in the definition's platformValues.
type workload struct {
	name, replicas, requests string
}

// platformDeployments are the Deployments each Platform definition runs on
// the system group. Karpenter's own two replicas are the only HA the
// platform keeps beyond CoreDNS and the EBS CSI controller (add-ons).
// cluster-autoscaler runs one replica.
var platformDeployments = map[string][]workload{
	"cluster-autoscaler": {{"cluster-autoscaler", "replicaCount", "resources.requests"}},
	"karpenter":          {{"karpenter", "replicas", "controller.resources.requests"}},
	"kyverno": {
		{"kyverno-admission", "admissionController.replicas", "admissionController.container.resources.requests"},
		{"kyverno-background", "backgroundController.replicas", "backgroundController.resources.requests"},
		{"kyverno-cleanup", "cleanupController.replicas", "cleanupController.resources.requests"},
		{"kyverno-reports", "reportsController.replicas", "reportsController.resources.requests"},
	},
	// With autoscaleEnabled the HPA's minimum is the replica count.
	"istiod":                {{"istiod", "autoscaleMin", "resources.requests"}},
	"istio-ingress-gateway": {{"istio-ingress-gateway", "autoscaling.minReplicas", "resources.requests"}},
	"external-secrets": {
		{"external-secrets", "replicaCount", "resources.requests"},
		{"external-secrets-webhook", "webhook.replicaCount", "webhook.resources.requests"},
		{"external-secrets-cert-controller", "certController.replicaCount", "certController.resources.requests"},
	},
	"capsule":                      {{"capsule", "replicaCount", "manager.resources.requests"}},
	"aws-load-balancer-controller": {{"aws-load-balancer-controller", "replicaCount", "resources.requests"}},
}

// platformDaemonSets are the DaemonSets of Platform definitions: one pod on
// every system node (requests in the definition's platformValues).
var platformDaemonSets = map[string]workload{
	"istio-cni": {name: "istio-cni", requests: "resources.requests"},
	"ztunnel":   {name: "ztunnel", requests: "resources.requests"},
}

// platformWithoutWorkloads install CRDs, policies or webhooks configuration
// only (kyverno-policies is a Tenant component). A new Platform definition must be listed here or above.
var platformWithoutWorkloads = map[string]bool{"istio": true, "istio-base": true}

// Add-ons are not in the catalog; their requests are the EKS add-on
// defaults (describe-addon-configuration / the upstream charts):
//
//	coredns                   2 x 100m CPU, 70Mi
//	aws-ebs-csi-driver        controller 2 x 6 containers x 10m, 40Mi
//	                          node DaemonSet 3 containers x 10m, 40Mi
//	vpc-cni (aws-node)        25m (the network policy agent adds none)
//	kube-proxy                100m
//	eks-pod-identity-agent    none set; 10m / 32Mi allowed
var (
	corednsEach        = usage{cpuMilli: 100, memMiB: 70, pods: 1}
	corednsReplicas    = int64(2)
	ebsCSIController   = usage{cpuMilli: 60, memMiB: 240, pods: 1}
	ebsCSIReplicas     = int64(2)
	addonDaemonSetsPer = []line{
		{"vpc-cni", 1, usage{25, 0, 1}},
		{"kube-proxy", 1, usage{100, 0, 1}},
		{"eks-pod-identity-agent", 1, usage{10, 32, 1}},
		{"ebs-csi-node", 1, usage{30, 120, 1}},
	}
)

// requestsAt reads the CPU and memory requests at a dotted path of a
// definition's platformValues; both must be set.
func requestsAt(t *testing.T, def, path string, values map[string]any) usage {
	t.Helper()
	node := valueAt(t, def, path, values)
	m, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("%s: platformValues %s is %T, want a map of requests", def, path, node)
	}
	cpu := quantityOf(t, def, path+".cpu", m["cpu"])
	mem := quantityOf(t, def, path+".memory", m["memory"])
	return usage{cpuMilli: cpu.MilliValue(), memMiB: mem.Value() / (1 << 20), pods: 1}
}

func valueAt(t *testing.T, def, path string, values map[string]any) any {
	t.Helper()
	var node any = values
	for _, key := range strings.Split(path, ".") {
		m, ok := node.(map[string]any)
		if !ok || m[key] == nil {
			t.Fatalf("%s: platformValues has no %q (set it explicitly: the test never guesses chart defaults)", def, path)
		}
		node = m[key]
	}
	return node
}

func quantityOf(t *testing.T, def, path string, v any) resource.Quantity {
	t.Helper()
	var s string
	switch n := v.(type) {
	case string:
		s = n
	case int, int64, float64:
		s = fmt.Sprint(n)
	default:
		t.Fatalf("%s: platformValues %s is %v (%T), want a quantity", def, path, v, v)
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		t.Fatalf("%s: %s: %v", def, path, err)
	}
	return q
}

func replicasAt(t *testing.T, def, path string, values map[string]any) int64 {
	t.Helper()
	q := quantityOf(t, def, path, valueAt(t, def, path, values))
	return q.Value()
}

// sizingTable is every request the shipped catalog and the add-ons place on
// the system group, for a group of `nodes` nodes.
func sizingTable(t *testing.T, nodes int64) []line {
	t.Helper()
	var rows []line
	for _, d := range catalogDefinitions(t) {
		if d.Management != component.Platform {
			continue
		}
		if deployments, ok := platformDeployments[d.Name]; ok {
			for _, w := range deployments {
				rows = append(rows, line{w.name, replicasAt(t, d.Name, w.replicas, d.PlatformValues),
					requestsAt(t, d.Name, w.requests, d.PlatformValues)})
			}
			continue
		}
		if w, ok := platformDaemonSets[d.Name]; ok {
			rows = append(rows, line{w.name, nodes, requestsAt(t, d.Name, w.requests, d.PlatformValues)})
			continue
		}
		if !platformWithoutWorkloads[d.Name] {
			t.Errorf("Platform definition %s is in no sizing list: add its Deployments and DaemonSets to platformDeployments / platformDaemonSets, or to platformWithoutWorkloads if it runs none", d.Name)
		}
	}
	rows = append(rows, line{"coredns (add-on)", corednsReplicas, corednsEach},
		line{"ebs-csi controller (add-on)", ebsCSIReplicas, ebsCSIController})
	for _, ds := range addonDaemonSetsPer {
		rows = append(rows, line{ds.name + " (add-on DaemonSet)", nodes, ds.each})
	}
	return rows
}

func TestSizingNamesEveryPlatformDefinition(t *testing.T) {
	// Every key of the lists is a Platform definition (no stale entries).
	platform := map[string]bool{}
	for _, d := range catalogDefinitions(t) {
		if d.Management == component.Platform {
			platform[d.Name] = true
		}
	}
	for name := range platformDeployments {
		if !platform[name] {
			t.Errorf("platformDeployments names %s, which is not a Platform definition", name)
		}
	}
	for name := range platformDaemonSets {
		if !platform[name] {
			t.Errorf("platformDaemonSets names %s, which is not a Platform definition", name)
		}
	}
	for name := range platformWithoutWorkloads {
		if !platform[name] {
			t.Errorf("platformWithoutWorkloads names %s, which is not a Platform definition", name)
		}
	}
}

// The default system group, at its MINIMUM size, holds the platform's
// requests with headroom (pods limit included).
func TestCatalogFitsTheDefaultSystemNodeGroup(t *testing.T) {
	group := config.DefaultNodeGroups().System
	shape, ok := instanceShapes[group.InstanceType]
	if !ok {
		t.Fatalf("default system instance type %q is not in instanceShapes: add its size", group.InstanceType)
	}
	nodes := int64(group.MinSize)
	nodeCPU, nodeMem := shape.allocatable()
	allocCPU, allocMem, maxPods := nodeCPU*nodes, nodeMem*nodes, shape.maxPods*nodes

	var sum usage
	var table strings.Builder
	for _, l := range sizingTable(t, nodes) {
		tot := l.total()
		sum = sum.plus(tot)
		fmt.Fprintf(&table, "  %-44s %2d x %4dm %5d MiB = %5dm %6d MiB %3d pods\n",
			l.name, l.replicas, l.each.cpuMilli, l.each.memMiB, tot.cpuMilli, tot.memMiB, tot.pods)
	}
	summary := fmt.Sprintf("%d x %s allocatable %dm / %d MiB / %d pods; requests %dm (%.0f%%) / %d MiB (%.0f%%) / %d pods (%.0f%%)\n%s",
		nodes, group.InstanceType, allocCPU, allocMem, maxPods,
		sum.cpuMilli, 100*float64(sum.cpuMilli)/float64(allocCPU),
		sum.memMiB, 100*float64(sum.memMiB)/float64(allocMem),
		sum.pods, 100*float64(sum.pods)/float64(maxPods), table.String())
	t.Log("\n" + summary)

	if float64(sum.cpuMilli) > systemGroupFillLimit*float64(allocCPU) {
		t.Errorf("CPU requests %dm exceed %.0f%% of the %dm allocatable\n%s", sum.cpuMilli, 100*systemGroupFillLimit, allocCPU, summary)
	}
	if float64(sum.memMiB) > systemGroupFillLimit*float64(allocMem) {
		t.Errorf("memory requests %d MiB exceed %.0f%% of the %d MiB allocatable\n%s", sum.memMiB, 100*systemGroupFillLimit, allocMem, summary)
	}
	if sum.pods > maxPods {
		t.Errorf("%d pods exceed the %d the nodes hold (%d x %d)\n%s", sum.pods, maxPods, nodes, shape.maxPods, summary)
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
	shape := instanceShapes[group.InstanceType]
	nodeCPU, nodeMem := shape.allocatable()
	maxNodes := int64(group.MaxSize)
	var perNodeCPU, perNodeMem int64
	var requested usage
	for _, l := range sizingTable(t, maxNodes) {
		requested = requested.plus(l.total())
	}
	for _, ds := range addonDaemonSetsPer {
		perNodeCPU += ds.each.cpuMilli
		perNodeMem += ds.each.memMiB
	}
	if requested.pods >= maxNodes*shape.maxPods || requested.cpuMilli >= nodeCPU*maxNodes || requested.memMiB >= nodeMem*maxNodes {
		t.Errorf("at the maximum of %d nodes the requests (%dm, %d MiB, %d pods) fill the group (%dm per node of DaemonSets %dm, %d MiB)",
			maxNodes, requested.cpuMilli, requested.memMiB, requested.pods, nodeCPU, perNodeCPU, perNodeMem)
	}
	// Each added node nets room: its DaemonSets leave allocatable CPU and memory.
	if nodeCPU-perNodeCPU <= 0 || nodeMem-perNodeMem <= 0 {
		t.Error("a node's add-on DaemonSets take all of it: adding nodes would not help")
	}
}

// Capacity is never added by the test: a smaller default group must fail.
func TestSizingTableIsNotEmpty(t *testing.T) {
	rows := sizingTable(t, 2)
	if len(rows) < 12 {
		t.Fatalf("sizing table has %d rows, want the whole catalog", len(rows))
	}
	for _, l := range rows {
		if l.replicas < 1 || l.each.cpuMilli <= 0 {
			t.Errorf("%s: %d replicas of %dm: every row must request something", l.name, l.replicas, l.each.cpuMilli)
		}
	}
}
