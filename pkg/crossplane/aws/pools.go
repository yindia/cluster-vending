package aws

// The node pools of a cluster (docs/design/karpenter-node-groups.md): the
// default managed node group ("system") runs Karpenter, CoreDNS, every
// platform component, the tenant's components and the EKS add-on
// controllers, at a fixed size; a user's own Karpenter node groups run
// their workloads. The system group's nodes carry the label
// PoolLabelKey=system and are tainted with it (NoSchedule); a user group's
// nodes carry the label PoolLabelKey=<name>.
const (
	// PoolLabelKey is the node label (and taint key) that names a pool.
	PoolLabelKey = "node.cluster-vending.io/pool"
	// PoolSystem is the default managed node group, the platform's only
	// node group.
	PoolSystem = "system"
	// DefaultSystemPoolSize is the size of the default managed node group
	// (min = max = desired, no autoscaler) unless the admin sets another
	// (--system-node-group-size).
	DefaultSystemPoolSize = 2
	// MinSystemPoolSize is the least the admin may set (and the default):
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

// SystemPoolSize is the size the system group has when the admin sets size:
// size itself, or DefaultSystemPoolSize when it is below MinSystemPoolSize
// (unset).
func SystemPoolSize(size int) int {
	if size < MinSystemPoolSize {
		return DefaultSystemPoolSize
	}
	return size
}

// FixedSystemNodePool is plan with the default node group's placement and
// size the platform fixes: the system label and taint, min = max = desired
// = size (DefaultSystemPoolSize when size is below MinSystemPoolSize).
// Instance types, capacity type, AMI, disk and launch template are the
// plan's own (EKS cannot change them in place; these it can).
func FixedSystemNodePool(plan NodePoolPlan, size int) NodePoolPlan {
	size = SystemPoolSize(size)
	plan.Labels = PoolLabels(PoolSystem)
	plan.Taints = []TaintPlan{PoolTaint(PoolSystem)}
	plan.MinSize, plan.MaxSize, plan.Desired = size, size, size
	return plan
}
