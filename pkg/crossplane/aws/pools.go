package aws

// The node pools of a cluster (docs/architecture.md#node-layout): the
// default managed node group ("system") runs Karpenter, CoreDNS, every
// platform component, the tenant's components and the EKS add-on
// controllers, between a minimum and a maximum size that cluster-autoscaler
// moves it in; a user's own Karpenter node groups run
// their workloads. The system group's nodes carry the label
// PoolLabelKey=system and are tainted with it (NoSchedule); a user group's
// nodes carry the label PoolLabelKey=<name>.
const (
	// PoolLabelKey is the node label (and taint key) that names a pool.
	PoolLabelKey = "node.cluster-vending.io/pool"
	// PoolSystem is the default managed node group, the platform's only
	// node group.
	PoolSystem = "system"
	// PoolPlatform is the name of the platform pool (PROP-03): the Karpenter
	// NodePool and EC2NodeClass, and the value of its node label and taint.
	// It is not the legacy "platform" the legacy gate removes.
	PoolPlatform = "platform-pool"
	// DefaultSystemPoolMin is the size of the default managed node group at
	// creation, and its minimum, unless the admin sets another
	// (--system-node-group-min-size).
	DefaultSystemPoolMin = 2
	// DefaultSystemPoolMax is how far cluster-autoscaler may grow the group
	// unless the admin sets another (--system-node-group-max-size).
	DefaultSystemPoolMax = 6
	// MinSystemPoolSize is the least the admin may set as the minimum:
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

// SystemPoolBounds is the minimum and maximum size the system group has
// when the admin sets them: the minimum itself, or DefaultSystemPoolMin
// when it is below MinSystemPoolSize (unset); the maximum itself, or
// DefaultSystemPoolMax when unset (not above 0) and never below the
// minimum.
func SystemPoolBounds(minSize, maxSize int) (int, int) {
	if minSize < MinSystemPoolSize {
		minSize = DefaultSystemPoolMin
	}
	if maxSize <= 0 {
		maxSize = max(DefaultSystemPoolMax, minSize)
	}
	return minSize, max(maxSize, minSize)
}

// FixedSystemNodePool is plan with the default node group's placement and
// size the platform fixes: the system label and taint, min and max from
// SystemPoolBounds, and desired = min. Desired is only the size at creation:
// the apply sends it in initProvider (DesiredSizeAtCreateOnly), so after
// that the node count is cluster-autoscaler's. Instance types, capacity
// type, AMI, disk and launch template are the plan's own (EKS cannot change
// them in place; these it can).
func FixedSystemNodePool(plan NodePoolPlan, minSize, maxSize int) NodePoolPlan {
	minSize, maxSize = SystemPoolBounds(minSize, maxSize)
	plan.Labels = PoolLabels(PoolSystem)
	plan.Taints = []TaintPlan{PoolTaint(PoolSystem)}
	plan.MinSize, plan.MaxSize, plan.Desired = minSize, maxSize, minSize
	return plan
}
