package aws

// The node pools of a cluster (docs/architecture.md#node-layout): the
// default managed node group ("system") runs phase 1 of the install order,
// Karpenter and the EKS add-on controllers, at a static size the platform
// admin sets (nothing autoscales it); Karpenter's platform pool (PoolPlatform) runs every other
// platform component and the tenant's components; a user's own Karpenter
// node groups run their workloads. The system group's nodes carry the label
// PoolLabelKey=system and are tainted with it (NoSchedule); a user group's
// nodes carry the label PoolLabelKey=<name>.
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
	// DefaultSystemInstanceType is the default group's instance type for a
	// new cluster unless the admin sets another (--system-node-group-
	// instance-type) and the spec names none. An existing cluster keeps
	// the instance type stored in its spec (no node group replacement).
	DefaultSystemInstanceType = "t3.medium"

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

// FixedSystemNodePool is plan with the default node group's placement and
// size the platform fixes: the system label and taint, and one static size
// (SystemPoolSize) as min, max and desired. Nothing autoscales the group
// (Karpenter scales every other pool), and the apply sends desired in
// forProvider (ClusterSet), so a change of the size, or a group that grew
// under the former cluster-autoscaler, is applied in place. Instance types,
// capacity type, AMI, disk and launch template are the plan's own (EKS
// cannot change them in place; the size it can).
func FixedSystemNodePool(plan NodePoolPlan, size int) NodePoolPlan {
	size = SystemPoolSize(size)
	plan.Labels = PoolLabels(PoolSystem)
	plan.Taints = []TaintPlan{PoolTaint(PoolSystem)}
	plan.MinSize, plan.MaxSize, plan.Desired = size, size, size
	return plan
}
