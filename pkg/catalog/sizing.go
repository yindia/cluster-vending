package catalog

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/config"
)

// The system managed node group runs Karpenter, cluster-autoscaler, the EKS
// add-on controllers, every platform component and the DaemonSets of each
// node. CheckSystemGroupSizing fails when what the enabled platform
// components REQUEST there no longer fits the configured group at its
// MINIMUM size with headroom: a chart bump, a definition edit or a smaller
// nodeGroups.system cannot silently outgrow it (docs/architecture.md#node-layout,
// "Sizing the system group"). The group is autoscaled: when pending pods
// need room, cluster-autoscaler adds nodes up to MaxSize, so what does not
// fit at the minimum is a cost (more nodes), not an outage, but the
// configured group should be one that needs none.
//
// Only what a definition's platformValues set is counted, so a component's
// requests and replicas must be explicit there: a chart default is never
// guessed (a missing key is a problem). Key names were checked against the
// pinned chart's values with `helm show values` / `helm template`.

// SystemGroupFillLimit is the share of the group's allocatable CPU and
// memory the platform may request; the rest is for tenant components,
// rollouts (surge pods) and growth.
const SystemGroupFillLimit = 0.85

// Shape is an instance type's size and what EKS lets pods use.
type Shape struct {
	VCPU    int64 // cores
	MemMiB  int64 // instance memory, MiB
	MaxPods int64 // EKS max pods with the VPC CNI defaults (ENIs x (IPs - 1) + 2)
}

// instanceShapes are the types an operator may reasonably pick for the
// system group.
var instanceShapes = map[string]Shape{
	"t3.small":   {VCPU: 2, MemMiB: 2048, MaxPods: 11},
	"t3.medium":  {VCPU: 2, MemMiB: 4096, MaxPods: 17},
	"t3.large":   {VCPU: 2, MemMiB: 8192, MaxPods: 35},
	"t3.xlarge":  {VCPU: 4, MemMiB: 16384, MaxPods: 58},
	"t3.2xlarge": {VCPU: 8, MemMiB: 32768, MaxPods: 58},
	"m5.large":   {VCPU: 2, MemMiB: 8192, MaxPods: 29},
	"m5.xlarge":  {VCPU: 4, MemMiB: 16384, MaxPods: 58},
	"m5.2xlarge": {VCPU: 8, MemMiB: 32768, MaxPods: 58},
	"m6i.large":  {VCPU: 2, MemMiB: 8192, MaxPods: 29},
	"m6i.xlarge": {VCPU: 4, MemMiB: 16384, MaxPods: 58},
}

// InstanceShape is the size of an instance type the sizing rule knows.
func InstanceShape(instanceType string) (Shape, bool) {
	s, ok := instanceShapes[instanceType]
	return s, ok
}

// vmMemoryOverheadPercent is the memory the OS and hypervisor take before
// the kubelet sees it (Karpenter's default assumption, 7.5%).
const vmMemoryOverheadPercent = 0.075

// evictionHardMiB is the kubelet's memory.available eviction threshold.
const evictionHardMiB = 100

// Allocatable is what pods may request on one node of the shape, by the EKS
// AL2023 (nodeadm) reservations:
//
//	kube-reserved memory = 255 MiB + 11 MiB x maxPods
//	kube-reserved CPU    = 6% of the 1st core, 1% of the 2nd, 0.5% of the
//	                       next 2, 0.25% of the rest
//	system eviction      = 100 MiB (memory.available)
//
// Memory capacity is the instance memory less vmMemoryOverheadPercent.
func (s Shape) Allocatable() (cpuMilli, memMiB int64) {
	reserved := int64(60) // 6% of the 1st core
	if s.VCPU > 1 {
		reserved += 10 // 1% of the 2nd
	}
	if s.VCPU > 2 {
		reserved += int64(float64(min(s.VCPU-2, 2)) * 1000 * 0.005)
	}
	if s.VCPU > 4 {
		reserved += int64(float64(s.VCPU-4) * 1000 * 0.0025)
	}
	capacity := int64(float64(s.MemMiB) * (1 - vmMemoryOverheadPercent))
	memReserved := 255 + 11*s.MaxPods + evictionHardMiB
	return s.VCPU*1000 - reserved, capacity - memReserved
}

// Usage is a CPU (millicores) and memory (MiB) request and a pod count.
type Usage struct {
	CPUMilli, MemMiB, Pods int64
}

// Times is the usage of n copies.
func (u Usage) Times(n int64) Usage { return Usage{u.CPUMilli * n, u.MemMiB * n, u.Pods * n} }

// Plus is the sum of two usages.
func (u Usage) Plus(o Usage) Usage {
	return Usage{u.CPUMilli + o.CPUMilli, u.MemMiB + o.MemMiB, u.Pods + o.Pods}
}

// SizingLine is one row of the sizing table.
type SizingLine struct {
	Name     string
	Replicas int64
	Each     Usage // one pod
}

// Total is the usage of all the row's pods.
func (l SizingLine) Total() Usage { return l.Each.Times(l.Replicas) }

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
// only (kyverno-policies is a Tenant component). A new Platform definition
// must be listed here or above.
var platformWithoutWorkloads = map[string]bool{"istio": true, "istio-base": true}

// SizedPlatformNames are the definitions the sizing table knows: those with
// Deployments, with DaemonSets, and those that run no workload.
func SizedPlatformNames() (deployments, daemonSets, withoutWorkloads []string) {
	for n := range platformDeployments {
		deployments = append(deployments, n)
	}
	for n := range platformDaemonSets {
		daemonSets = append(daemonSets, n)
	}
	for n := range platformWithoutWorkloads {
		withoutWorkloads = append(withoutWorkloads, n)
	}
	slices.Sort(deployments)
	slices.Sort(daemonSets)
	slices.Sort(withoutWorkloads)
	return deployments, daemonSets, withoutWorkloads
}

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
	corednsEach        = Usage{CPUMilli: 100, MemMiB: 70, Pods: 1}
	corednsReplicas    = int64(2)
	ebsCSIController   = Usage{CPUMilli: 60, MemMiB: 240, Pods: 1}
	ebsCSIReplicas     = int64(2)
	addonDaemonSetsPer = []SizingLine{
		{"vpc-cni", 1, Usage{25, 0, 1}},
		{"kube-proxy", 1, Usage{100, 0, 1}},
		{"eks-pod-identity-agent", 1, Usage{10, 32, 1}},
		{"ebs-csi-node", 1, Usage{30, 120, 1}},
	}
)

// AddonDaemonSetsPerNode are the add-on DaemonSets: what each node adds.
func AddonDaemonSetsPerNode() []SizingLine { return slices.Clone(addonDaemonSetsPer) }

// SizingTable is every request the enabled platform components (defs, with
// what is off left out) and the add-ons place on the system group, for a
// group of nodes nodes, and what could not be counted: a requests or
// replicas key a definition's platformValues lack, and a Platform
// definition no list names.
func SizingTable(defs []component.Definition, nodes int64) ([]SizingLine, []string) {
	var rows []SizingLine
	var problems []string
	off := component.Off(defs)
	ordered := slices.Clone(defs)
	slices.SortFunc(ordered, func(a, b component.Definition) int { return strings.Compare(a.Name, b.Name) })
	for _, d := range ordered {
		if _, isOff := off[d.Name]; isOff || d.Management != component.Platform {
			continue
		}
		if deployments, ok := platformDeployments[d.Name]; ok {
			for _, w := range deployments {
				replicas, err := replicasAt(d, w.replicas)
				if err != nil {
					problems = append(problems, err.Error())
					continue
				}
				each, err := requestsAt(d, w.requests)
				if err != nil {
					problems = append(problems, err.Error())
					continue
				}
				rows = append(rows, SizingLine{w.name, replicas, each})
			}
			continue
		}
		if w, ok := platformDaemonSets[d.Name]; ok {
			each, err := requestsAt(d, w.requests)
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}
			rows = append(rows, SizingLine{w.name, nodes, each})
			continue
		}
		if !platformWithoutWorkloads[d.Name] {
			problems = append(problems, fmt.Sprintf("Platform definition %s is in no sizing list: add its Deployments and DaemonSets to platformDeployments / platformDaemonSets (pkg/catalog/sizing.go), or to platformWithoutWorkloads if it runs none", d.Name))
		}
	}
	rows = append(rows, SizingLine{"coredns (add-on)", corednsReplicas, corednsEach},
		SizingLine{"ebs-csi controller (add-on)", ebsCSIReplicas, ebsCSIController})
	for _, ds := range addonDaemonSetsPer {
		rows = append(rows, SizingLine{ds.Name + " (add-on DaemonSet)", nodes, ds.Each})
	}
	return rows, problems
}

// CheckSystemGroupSizing reports what keeps the enabled platform
// components and the add-ons from fitting the system group at its minimum
// size: CPU and memory requests above SystemGroupFillLimit of what the
// nodes allocate, more pods than they hold, an instance type the rule has
// no size for, and what SizingTable could not count. The summary is the
// sizing table, for a log. Nothing is reported for a group that fits.
func CheckSystemGroupSizing(defs []component.Definition, group config.SystemNodeGroup) (problems []string, summary string) {
	shape, ok := instanceShapes[group.InstanceType]
	if !ok {
		return []string{fmt.Sprintf("nodeGroups.system.instanceType %q is not an instance type the sizing rule knows (%s): add its size to instanceShapes (pkg/catalog/sizing.go)",
			group.InstanceType, strings.Join(shapeNames(), ", "))}, ""
	}
	nodes := int64(group.MinSize)
	nodeCPU, nodeMem := shape.Allocatable()
	allocCPU, allocMem, maxPods := nodeCPU*nodes, nodeMem*nodes, shape.MaxPods*nodes

	rows, problems := SizingTable(defs, nodes)
	var sum Usage
	var table strings.Builder
	for _, l := range rows {
		tot := l.Total()
		sum = sum.Plus(tot)
		fmt.Fprintf(&table, "  %-44s %2d x %4dm %5d MiB = %5dm %6d MiB %3d pods\n",
			l.Name, l.Replicas, l.Each.CPUMilli, l.Each.MemMiB, tot.CPUMilli, tot.MemMiB, tot.Pods)
	}
	summary = fmt.Sprintf("%d x %s allocatable %dm / %d MiB / %d pods; requests %dm (%.0f%%) / %d MiB (%.0f%%) / %d pods (%.0f%%)\n%s",
		nodes, group.InstanceType, allocCPU, allocMem, maxPods,
		sum.CPUMilli, 100*float64(sum.CPUMilli)/float64(allocCPU),
		sum.MemMiB, 100*float64(sum.MemMiB)/float64(allocMem),
		sum.Pods, 100*float64(sum.Pods)/float64(maxPods), table.String())

	if float64(sum.CPUMilli) > SystemGroupFillLimit*float64(allocCPU) {
		problems = append(problems, fmt.Sprintf("system node group: CPU requests %dm exceed %.0f%% of the %dm allocatable by %d x %s (nodeGroups.system minSize, instanceType)",
			sum.CPUMilli, 100*SystemGroupFillLimit, allocCPU, nodes, group.InstanceType))
	}
	if float64(sum.MemMiB) > SystemGroupFillLimit*float64(allocMem) {
		problems = append(problems, fmt.Sprintf("system node group: memory requests %d MiB exceed %.0f%% of the %d MiB allocatable by %d x %s (nodeGroups.system minSize, instanceType)",
			sum.MemMiB, 100*SystemGroupFillLimit, allocMem, nodes, group.InstanceType))
	}
	if sum.Pods > maxPods {
		problems = append(problems, fmt.Sprintf("system node group: %d pods exceed the %d the nodes hold (%d x %d)", sum.Pods, maxPods, nodes, shape.MaxPods))
	}
	return problems, summary
}

func shapeNames() []string {
	names := make([]string, 0, len(instanceShapes))
	for n := range instanceShapes {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// requestsAt reads the CPU and memory requests at a dotted path of a
// definition's platformValues; both must be set.
func requestsAt(d component.Definition, path string) (Usage, error) {
	node, err := valueAt(d, path)
	if err != nil {
		return Usage{}, err
	}
	m, ok := node.(map[string]any)
	if !ok {
		return Usage{}, fmt.Errorf("%s: platformValues %s is %T, want a map of requests", d.Name, path, node)
	}
	cpu, err := quantityOf(d.Name, path+".cpu", m["cpu"])
	if err != nil {
		return Usage{}, err
	}
	mem, err := quantityOf(d.Name, path+".memory", m["memory"])
	if err != nil {
		return Usage{}, err
	}
	return Usage{CPUMilli: cpu.MilliValue(), MemMiB: mem.Value() / (1 << 20), Pods: 1}, nil
}

func valueAt(d component.Definition, path string) (any, error) {
	var node any = d.PlatformValues
	for _, key := range strings.Split(path, ".") {
		m, ok := node.(map[string]any)
		if !ok || m[key] == nil {
			return nil, fmt.Errorf("%s: platformValues has no %q (set it explicitly: the sizing rule never guesses chart defaults)", d.Name, path)
		}
		node = m[key]
	}
	return node, nil
}

func quantityOf(def, path string, v any) (resource.Quantity, error) {
	var s string
	switch n := v.(type) {
	case string:
		s = n
	case int, int64, float64:
		s = fmt.Sprint(n)
	default:
		return resource.Quantity{}, fmt.Errorf("%s: platformValues %s is %v (%T), want a quantity", def, path, v, v)
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("%s: %s: %w", def, path, err)
	}
	return q, nil
}

func replicasAt(d component.Definition, path string) (int64, error) {
	v, err := valueAt(d, path)
	if err != nil {
		return 0, err
	}
	q, err := quantityOf(d.Name, path, v)
	if err != nil {
		return 0, err
	}
	return q.Value(), nil
}
