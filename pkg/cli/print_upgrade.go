package cli

import (
	"fmt"
	"strings"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The steps of an upgrade, in the order they run.
var upgradeSteps = []struct {
	phase cloudv1.ClusterUpgradePhase
	name  string
}{
	{cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE, "control plane"},
	{cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODE_GROUPS, "node groups"},
	{cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_ADDONS, "add-ons"},
	{cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_PLATFORM_COMPONENTS, "platform components"},
	{cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_TENANT_COMPONENTS, "tenant components"},
}

// upgradeLine is cluster get's Upgrade line: from and to, then every step
// in order, done ones ticked, the current one with what it waits for, e.g.
// "1.36 → 1.37: control plane ✓, node groups ✓, add-ons (coredns,
// vpc-cni) …, platform components, tenant components".
func upgradeLine(u *cloudv1.ClusterUpgrade) string {
	phase := u.GetPhase()
	if phase == cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS {
		phase = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODE_GROUPS // as an older controller reported it
	}
	current := -1
	for i, s := range upgradeSteps {
		if s.phase == phase {
			current = i
		}
	}
	parts := make([]string, 0, len(upgradeSteps))
	for i, s := range upgradeSteps {
		switch {
		case current < 0 || i > current:
			parts = append(parts, s.name)
		case i < current:
			parts = append(parts, s.name+" ✓")
		default:
			parts = append(parts, currentStep(s.name, u))
		}
	}
	return fmt.Sprintf("%s → %s: %s", orNone(u.GetFromVersion()), orNone(u.GetToVersion()), strings.Join(parts, ", "))
}

// currentStep is the step under way: what it waits for, and whether it is
// recovering or halted.
func currentStep(name string, u *cloudv1.ClusterUpgrade) string {
	var waiting []string
	switch u.GetPhase() {
	case cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODE_GROUPS, cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS:
		for _, g := range u.GetNodeGroups() {
			state := strings.ToLower(word(g.GetState().String(), "MANAGED_RESOURCE_STATE_"))
			if g.GetFromVersion() == "" || g.GetFromVersion() == g.GetToVersion() {
				waiting = append(waiting, fmt.Sprintf("%s on %s %s", g.GetName(), g.GetToVersion(), state))
				continue
			}
			waiting = append(waiting, fmt.Sprintf("%s %s → %s %s", g.GetName(), g.GetFromVersion(), g.GetToVersion(), state))
		}
	case cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_ADDONS:
		waiting = u.GetAddonsBehind()
	case cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_PLATFORM_COMPONENTS, cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_TENANT_COMPONENTS:
		waiting = u.GetComponentsBehind()
	}
	out := name
	if len(waiting) > 0 {
		out += " (" + strings.Join(waiting, ", ") + ")"
	}
	switch {
	case u.GetHalted():
		return out + " halted"
	case u.GetRecoveries() > 0:
		return out + fmt.Sprintf(" recovering (%d of 3) …", u.GetRecoveries())
	}
	return out + " …"
}
