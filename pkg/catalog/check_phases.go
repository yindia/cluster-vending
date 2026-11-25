package catalog

import (
	"github.com/yindia/cluster-vending/pkg/component"
)

// checkPhases are the install-order rules of where a component runs
// (docs/architecture.md#node-layout, "Install order"; component.InstallPhase):
//
//   - a definition runs on one pool: ${system.*} or ${platform.*}, not both;
//   - karpenter is never on the platform pool, which it creates, and it is
//     the only definition on the system group: that group runs Karpenter
//     and the EKS add-ons, everything else runs on Karpenter's pool;
//   - a platform component on the system group (phase 1) requires nothing
//     on the platform pool (phase 2): the pool waits for Karpenter, so phase
//     1 must never wait for the pool.
func checkPhases(defs []component.Definition) []Problem {
	var problems []Problem
	phases := make(map[string]component.InstallPhase, len(defs))
	for _, d := range defs {
		phase, err := component.InstallPhaseOf(d)
		if err != nil {
			problems = append(problems, problemf("ComponentDefinition", d.Name, "%v", err))
			continue
		}
		phases[d.Name] = phase
		if d.Name == karpenterName && phase == component.PhasePlatformPool {
			problems = append(problems, problemf("ComponentDefinition", d.Name,
				"karpenter must not run on the platform pool (${platform.*}): it launches that pool's nodes; use ${system.*}"))
		}
		if d.Name != karpenterName && phase == component.PhaseSystemGroup {
			problems = append(problems, problemf("ComponentDefinition", d.Name,
				"only karpenter runs on the system group (${system.*}), beside the EKS add-ons: use ${platform.*}, Karpenter's platform pool"))
		}
	}
	for _, d := range defs {
		if !d.IsPlatform() || phases[d.Name] != component.PhaseSystemGroup {
			continue
		}
		for _, r := range d.Requires {
			if phases[r] == component.PhasePlatformPool {
				problems = append(problems, problemf("ComponentDefinition", d.Name,
					"runs on the system group (phase 1) but requires %s, which runs on the platform pool (phase 2): phase 1 cannot wait for the pool", r))
			}
		}
	}
	return problems
}
