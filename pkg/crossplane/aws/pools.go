package aws

import "slices"

// The node pools of a cluster (docs/architecture.md#node-layout): the
// default managed node group ("system") runs phase 1 of the install order,
// Karpenter and the EKS add-on controllers, at a static size the platform
// admin sets (nothing autoscales it); Karpenter's platform pool (PoolPlatform) runs every other
// platform component and the tenant's components. Both are the platform's,
// from its config (nodeGroups): tenants define no node groups. The system
// group's nodes carry the label PoolLabelKey=system and are tainted with it
// (NoSchedule).
const (
	// PoolLabelKey is the node label (and taint key) that names a pool.
	PoolLabelKey = "node.cluster-vending.io/pool"
	// PoolSystem is the default managed node group, the platform's only
	// EKS managed node group.
	PoolSystem = "system"
	// PoolPlatform is the name of the platform pool (PROP-03): the Karpenter
	// NodePool and EC2NodeClass, and the value of its node label and taint.
	// It is not the legacy "platform" the legacy gate removes.
	PoolPlatform = "platform-pool"
	// DefaultSystemPoolSize is the static size of the default managed node
	// group unless the admin sets another (--system-node-group-min-size).
	DefaultSystemPoolSize = 2
	// MinSystemPoolSize is the least the admin may set as the size:
	// Karpenter runs two replicas with anti-affinity, one on each of two
	// nodes.
	MinSystemPoolSize = 2
	// DefaultSystemInstanceType is the system group's instance type unless
	// the admin sets others (nodeGroups.system.instanceTypes).
	DefaultSystemInstanceType = "t3.medium"
	// DefaultSystemCapacityType and DefaultSystemAMIType are the system
	// group's capacity and AMI type unless the admin sets others (EKS
	// spellings).
	DefaultSystemCapacityType = "ON_DEMAND"
	DefaultSystemAMIType      = "AL2023_x86_64_STANDARD"

	taintNoSchedule = "NO_SCHEDULE"
)

// PoolLabels are the labels of a pool's nodes, which pods select with.
func PoolLabels(pool string) map[string]string { return map[string]string{PoolLabelKey: pool} }

// PoolTaint is the taint that keeps other pods off the pool's nodes.
func PoolTaint(pool string) TaintPlan {
	return TaintPlan{Key: PoolLabelKey, Value: pool, Effect: taintNoSchedule}
}

// PoolTolerations is the toleration of PoolTaint(pool), as a pod spec
// takes it.
func PoolTolerations(pool string) []map[string]any {
	return TolerationsOf([]TaintPlan{PoolTaint(pool)})
}

// SystemPoolSize is the static size of the system group for the admin's
// setting: the size itself, or DefaultSystemPoolSize when it is below
// MinSystemPoolSize (unset).
func SystemPoolSize(size int) int {
	if size < MinSystemPoolSize {
		return DefaultSystemPoolSize
	}
	return size
}

// SystemNodeGroup is the platform admin's settings of the system group
// (nodeGroups.system), the same for every cluster the platform created.
// Zero values are the defaults.
type SystemNodeGroup struct {
	// Size is the static size (SystemPoolSize).
	Size          int
	InstanceTypes []string
	// CapacityType and AMIType are EKS spellings (ON_DEMAND, SPOT;
	// AL2023_x86_64_STANDARD, ...).
	CapacityType, AMIType string
	// DiskSizeGiB is the nodes' disk; 0: EKS's default.
	DiskSizeGiB int
}

// SystemNodePool is the system group of s: the system label and taint, one
// static size (SystemPoolSize) as min, max and desired, and the admin's
// instance types, capacity type, AMI type and disk. Nothing autoscales the
// group (Karpenter scales the platform pool), and the apply sends desired
// in forProvider (ClusterSet), so a change of the size is applied in place.
// EKS cannot change the instance types, capacity type, AMI type or disk in
// place: a change of one of them replaces the group (a new one is created,
// the old one retired once the new one is Ready).
func SystemNodePool(s SystemNodeGroup) NodePoolPlan {
	size := SystemPoolSize(s.Size)
	types := slices.Clone(s.InstanceTypes)
	if len(types) == 0 {
		types = []string{DefaultSystemInstanceType}
	}
	return NodePoolPlan{InstanceTypes: types, CapacityType: orDefault(s.CapacityType, DefaultSystemCapacityType),
		AMIType: orDefault(s.AMIType, DefaultSystemAMIType), DiskSizeGB: s.DiskSizeGiB,
		MinSize: size, MaxSize: size, Desired: size,
		Labels: PoolLabels(PoolSystem), Taints: []TaintPlan{PoolTaint(PoolSystem)}}
}
