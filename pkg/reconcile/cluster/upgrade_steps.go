package cluster

import (
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The steps of an upgrade, in the order a cluster is created.
const (
	stepUnspecified   = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_UNSPECIFIED
	stepControlPlane  = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE
	stepNodesAndAddon = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS
	stepNodeGroups    = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODE_GROUPS
	stepAddonsPhase   = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_ADDONS
	stepPlatformComps = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_PLATFORM_COMPONENTS
	stepTenantComps   = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_TENANT_COMPONENTS
)

// settledSteps is what one pass sees of an upgrade's steps, each as AWS
// and Helm report it (versions from status.atProvider, Ready, deployed).
type settledSteps struct {
	// controlPlane: the EKS cluster reports the target.
	controlPlane bool
	// nodes: every node group reports the target, is applied at it and
	// is READY with its provider caught up.
	nodes bool
	// addons: the cluster's generation is applied (the add-ons are no
	// longer held at their versions) and every add-on is READY on the
	// version it is applied at.
	addons bool
	// platform, tenant: every platform (tenant) component is READY.
	platform, tenant bool
}

// stepOf is a recorded step as it reads now: NODES_AND_ADDONS, recorded
// by builds that had one step for both, is the node groups' step.
func stepOf(u *cloudv1.ClusterUpgrade) cloudv1.ClusterUpgradePhase {
	if p := u.GetPhase(); p != stepNodesAndAddon {
		return p
	}
	return stepNodeGroups
}

// nextStep is the step of an upgrade after this pass, from the step the
// last pass recorded (prev, nil when no upgrade to this target was
// recorded) and what the pass sees; applied says the step's changes were
// applied by this pass. The control plane goes first, whatever was
// recorded. After it, the upgrade moves one step at a time: a step is
// left only once it is settled and was applied in an earlier pass, so a
// pass that applied a change and still reads the old, settled state does
// not move on. stepUnspecified: no upgrade (none recorded and nothing
// behind, or the last step done).
func nextStep(prev *cloudv1.ClusterUpgrade, s settledSteps, nodesBehind bool) (cloudv1.ClusterUpgradePhase, bool) {
	if !s.controlPlane {
		return stepControlPlane, true
	}
	step := stepOf(prev)
	switch {
	case prev == nil && nodesBehind:
		return stepNodeGroups, false // first seen after the control plane
	case prev == nil, step == stepUnspecified:
		return stepUnspecified, false
	case step == stepControlPlane:
		return stepNodeGroups, false
	}
	if !prev.GetApplied() || !s.settled(step) {
		return step, true
	}
	return after(step), false
}

func (s settledSteps) settled(step cloudv1.ClusterUpgradePhase) bool {
	switch step {
	case stepNodeGroups:
		return s.nodes
	case stepAddonsPhase:
		return s.addons
	case stepPlatformComps:
		return s.platform
	case stepTenantComps:
		return s.tenant
	default:
		return true
	}
}

// after is the step that follows step; stepUnspecified after the last.
func after(step cloudv1.ClusterUpgradePhase) cloudv1.ClusterUpgradePhase {
	switch step {
	case stepNodeGroups:
		return stepAddonsPhase
	case stepAddonsPhase:
		return stepPlatformComps
	case stepPlatformComps:
		return stepTenantComps
	default:
		return stepUnspecified
	}
}

// reached reports whether an upgrade at step has reached want (or gone past
// it).
func reached(step, want cloudv1.ClusterUpgradePhase) bool {
	order := map[cloudv1.ClusterUpgradePhase]int{stepControlPlane: 1, stepNodesAndAddon: 2, stepNodeGroups: 2,
		stepAddonsPhase: 3, stepPlatformComps: 4, stepTenantComps: 5}
	return order[step] >= order[want]
}
