package aws

// The node pools of a cluster (docs/design/karpenter-node-groups.md): the
// default managed node group ("system") runs Karpenter and CoreDNS, the
// Karpenter NodePool "platform" runs the platform's and the tenant's
// components, and a user's own Karpenter node groups run their workloads.
// Each pool's nodes carry the label PoolLabelKey=<pool>; the system and
// platform pools are also tainted with it (NoSchedule).
const (
	// PoolLabelKey is the node label (and taint key) that names a pool.
	PoolLabelKey = "node.cluster-vending.io/pool"
	// PoolSystem is the default managed node group; PoolPlatform the
	// platform's Karpenter NodePool (and EC2NodeClass).
	PoolSystem   = "system"
	PoolPlatform = "platform"
	// SystemPoolSize is the fixed size of the default managed node group:
	// Karpenter runs two replicas, one on each node.
	SystemPoolSize = 2
	// DefaultSystemInstanceType is the default group's instance type when
	// the spec names none (new clusters only: an existing cluster keeps the
	// instance type stored in its spec). A t3.medium fits Karpenter's
	// 1 CPU / 1Gi request per replica and CoreDNS.
	DefaultSystemInstanceType = "t3.medium"

	taintNoSchedule = "NO_SCHEDULE"
)

// The platform NodePool's instance choice: the platform's own, small and
// burstable, whatever the admin's allowlist for user groups says. Change
// them here.
var (
	PlatformInstanceFamilies = []string{"t3"}
	PlatformInstanceSizes    = []string{"medium", "large", "xlarge"}
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

// FixedSystemNodePool is plan with the default node group's placement and
// size the platform fixes: the system label and taint, min = max = desired
// = SystemPoolSize. Instance types, capacity type, AMI, disk and launch
// template are the plan's own (EKS cannot change them in place; these it
// can).
func FixedSystemNodePool(plan NodePoolPlan) NodePoolPlan {
	plan.Labels = PoolLabels(PoolSystem)
	plan.Taints = []TaintPlan{PoolTaint(PoolSystem)}
	plan.MinSize, plan.MaxSize, plan.Desired = SystemPoolSize, SystemPoolSize, SystemPoolSize
	return plan
}
