package catalog

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/config"
)

// The sizing rules of PROP-03 4.5 (docs/proposals/PROP-03-component-placement.md):
//
//   - the bootstrap set (Karpenter, CoreDNS, the EBS CSI controller and the
//     DaemonSets of each node) against the bootstrap group at its fixed
//     size, counting CPU, memory and the pods a node holds (17 on a
//     t3.medium);
//   - the platform set against the platform pool: its requests must fit the
//     pool's limits with headroom, and every pod must fit the largest shape
//     the pool may launch.
//
// CheckSystemGroupSizing sizes what runs on the system group (everything
// not placed on the platform pool, with the add-ons); CheckBootstrapSizing
// the narrower bootstrap set of PROP-03 at the group's fixed size.

// memoryClassFactor is how far apart the memory of the types of the
// bootstrap group may be: a list is a fallback for a capacity shortage, so
// its types are of one class.
const memoryClassFactor = 2

// smallestShape is the shape the bootstrap sizing uses for a list of
// instance types: the least of each dimension. The list must be non-empty,
// every type known, of one vCPU count and within memoryClassFactor of
// memory. The name is the smallest type (by memory, then name).
func smallestShape(types []string) (Shape, string, []string) {
	if len(types) == 0 {
		return Shape{}, "", []string{"nodeGroups.system.instanceTypes names no instance type"}
	}
	var problems []string
	var least Shape
	var name string
	var maxMem, nameMem int64
	for _, t := range types {
		shape, ok := instanceShapes[t]
		if !ok {
			problems = append(problems, fmt.Sprintf("nodeGroups.system.instanceTypes %q is not an instance type the sizing rule knows (%s): add its size to instanceShapes (pkg/catalog/sizing.go)",
				t, strings.Join(shapeNames(), ", ")))
			continue
		}
		if name == "" || shape.MemMiB < nameMem {
			name, nameMem = t, shape.MemMiB
		}
		if least == (Shape{}) {
			least = shape
		} else {
			if shape.VCPU != least.VCPU {
				problems = append(problems, fmt.Sprintf("nodeGroups.system.instanceTypes mixes vCPU classes (%s has %d vCPU, another %d): a list is a fallback for one class", t, shape.VCPU, least.VCPU))
			}
			least = Shape{VCPU: min(least.VCPU, shape.VCPU), MemMiB: min(least.MemMiB, shape.MemMiB), MaxPods: min(least.MaxPods, shape.MaxPods)}
		}
		maxMem = max(maxMem, shape.MemMiB)
	}
	if len(problems) == 0 && maxMem > memoryClassFactor*least.MemMiB {
		problems = append(problems, fmt.Sprintf("nodeGroups.system.instanceTypes mixes memory classes (%d MiB to %d MiB): a list is a fallback for one class", least.MemMiB, maxMem))
	}
	if len(problems) > 0 {
		return Shape{}, "", problems
	}
	return least, name, nil
}

// isBootstrapDefinition: the definitions of defs that run on the bootstrap
// group, the NodeProvisioner and those with DaemonSets on every node
// (CoreDNS and the EBS controller are add-ons, which SizingTable adds
// itself).
func isBootstrapDefinition(d component.Definition, defs []component.Definition) bool {
	return component.RoleOf(d, defs) == component.RoleNodeProvisioner || len(d.Sizing.DaemonSets) > 0
}

// CheckBootstrapSizing reports what keeps the bootstrap set from fitting
// the bootstrap group at its fixed size: CPU and memory requests above
// SystemGroupFillLimit of what the nodes allocate, more pods than the
// nodes hold (minSize x the pods of the smallest type), and an instance
// type list the rule cannot size. The summary is the sizing table.
func CheckBootstrapSizing(defs []component.Definition, group config.SystemNodeGroup) (problems []string, summary string) {
	shape, typeName, problems := smallestShape(group.InstanceTypes)
	if len(problems) > 0 {
		return problems, ""
	}
	var bootstrap []component.Definition
	for _, d := range defs {
		if isBootstrapDefinition(d, defs) {
			bootstrap = append(bootstrap, d)
		}
	}
	return fitGroup(bootstrap, func(SizingLine) bool { return true }, shape, typeName, int64(group.MinSize), "bootstrap group")
}

// PlatformFillLimit is the share of the pool's limits the platform set
// may request at its minimum replicas; the rest is for growth and surge.
const PlatformFillLimit = 0.85

// CheckPlatformSizing reports what keeps the enabled platform components
// (at their minimum replicas) from fitting the platform pool: requests
// above PlatformFillLimit of limits.cpu or limits.memory, a pod that fits
// no shape the pool may launch (families x sizes), and families the rule
// has no shape for. Tenant components have no requests in the catalog and
// are not counted yet.
func CheckPlatformSizing(defs []component.Definition, pool config.PlatformNodePool) (problems []string, summary string) {
	rows, rowProblems := SizingTable(defs, 1)
	problems = append(problems, rowProblems...)
	var sum Usage
	var table strings.Builder
	var kept []SizingLine
	onPool := poolRows(defs)
	for _, l := range rows {
		if !onPool(l) {
			continue
		}
		kept = append(kept, l)
		sum = sum.Plus(l.Total())
		fmt.Fprintf(&table, "  %-44s %2d x %4dm %5d MiB = %5dm %6d MiB\n", l.Name, l.Replicas, l.Each.CPUMilli, l.Each.MemMiB, l.Total().CPUMilli, l.Total().MemMiB)
	}
	limitCPU := pool.Limits.CPU * 1000
	limitMem := int64(0)
	if q, err := resource.ParseQuantity(pool.Limits.Memory); err == nil {
		limitMem = q.Value() / (1 << 20)
	}
	summary = fmt.Sprintf("platform pool limits %dm / %d MiB; requests %dm / %d MiB\n%s", limitCPU, limitMem, sum.CPUMilli, sum.MemMiB, table.String())
	if float64(sum.CPUMilli) > PlatformFillLimit*float64(limitCPU) {
		problems = append(problems, fmt.Sprintf("platform pool: CPU requests %dm exceed %.0f%% of nodeGroups.platform.limits.cpu (%dm)", sum.CPUMilli, 100*PlatformFillLimit, limitCPU))
	}
	if float64(sum.MemMiB) > PlatformFillLimit*float64(limitMem) {
		problems = append(problems, fmt.Sprintf("platform pool: memory requests %d MiB exceed %.0f%% of nodeGroups.platform.limits.memory (%d MiB)", sum.MemMiB, 100*PlatformFillLimit, limitMem))
	}
	shapes, shapeProblems := poolShapes(pool)
	problems = append(problems, shapeProblems...)
	if len(shapes) == 0 {
		return problems, summary
	}
	var bestCPU, bestMem int64
	for _, sh := range shapes {
		cpu, mem := sh.Allocatable()
		bestCPU, bestMem = max(bestCPU, cpu), max(bestMem, mem)
	}
	for _, l := range kept {
		if l.Each.CPUMilli > bestCPU || l.Each.MemMiB > bestMem {
			problems = append(problems, fmt.Sprintf("platform pool: %s requests %dm / %d MiB per pod, more than the largest shape the pool allows holds (%dm / %d MiB allocatable): it can never schedule (nodeGroups.platform instanceFamilies, instanceSizes)",
				l.Name, l.Each.CPUMilli, l.Each.MemMiB, bestCPU, bestMem))
		}
	}
	return problems, summary
}

// poolShapes are the known shapes of the pool's families x sizes. A
// family with no known shape is a problem; a size a family lacks is not.
func poolShapes(pool config.PlatformNodePool) ([]Shape, []string) {
	var shapes []Shape
	var problems []string
	for _, f := range pool.InstanceFamilies {
		found := false
		for _, sz := range pool.InstanceSizes {
			sh, ok := instanceShapes[f+"."+sz]
			if !ok {
				continue
			}
			found = true
			if pool.MaxPods > 0 {
				sh.MaxPods = int64(pool.MaxPods)
			}
			shapes = append(shapes, sh)
		}
		if !found {
			problems = append(problems, fmt.Sprintf("nodeGroups.platform: family %q has no size in %s the sizing rule knows: add its shapes to instanceShapes (pkg/catalog/sizing.go)", f, strings.Join(pool.InstanceSizes, ", ")))
		}
	}
	return shapes, problems
}
