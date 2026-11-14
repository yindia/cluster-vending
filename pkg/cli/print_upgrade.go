package cli

import (
	"fmt"
	"strings"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// upgradeLine is cluster get's Upgrade line: from, to and the step, then,
// once the control plane runs the new version, how each node group and
// the add-ons follow, e.g. "1.36 to 1.37 (NodesAndAddons): node group
// default 1.36 → 1.37 updating; add-ons current".
func upgradeLine(u *cloudv1.ClusterUpgrade) string {
	line := fmt.Sprintf("%s to %s (%s)", orNone(u.GetFromVersion()), orNone(u.GetToVersion()),
		word(u.GetPhase().String(), "CLUSTER_UPGRADE_PHASE_"))
	if u.GetPhase() != cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS {
		return line
	}
	var parts []string
	for _, g := range u.GetNodeGroups() {
		state := strings.ToLower(word(g.GetState().String(), "MANAGED_RESOURCE_STATE_"))
		if g.GetFromVersion() == "" || g.GetFromVersion() == g.GetToVersion() {
			parts = append(parts, fmt.Sprintf("node group %s on %s %s", g.GetName(), g.GetToVersion(), state))
			continue
		}
		parts = append(parts, fmt.Sprintf("node group %s %s → %s %s", g.GetName(), g.GetFromVersion(), g.GetToVersion(), state))
	}
	if behind := u.GetAddonsBehind(); len(behind) > 0 {
		parts = append(parts, "add-ons updating: "+strings.Join(behind, ", "))
	} else {
		parts = append(parts, "add-ons current")
	}
	return line + ": " + strings.Join(parts, "; ")
}
