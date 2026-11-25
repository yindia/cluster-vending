package component

import (
	"fmt"
	"slices"
	"strings"
)

// InstallPhase is where a definition's pods run, which decides when it can
// be installed (docs/architecture.md#node-layout, "Install order"):
//
//   - PhaseSystemGroup: its values use ${system.*}. It runs on the fixed
//     system managed node group, which exists before any component, so it
//     installs in phase 1 (Karpenter first, by sequence 0).
//   - PhasePlatformPool: its values use ${platform.*}. It runs on
//     Karpenter's platform pool, so it waits (phase 2) until Karpenter is
//     Ready and the pool's NodePool and EC2NodeClass are: until then its
//     placement placeholders have no value (WaitForPlatformPool).
//   - PhaseUnplaced: no placement of its own (CRDs, policies, DaemonSets
//     that tolerate every node): it is ordered by sequence and requires only.
type InstallPhase int

const (
	PhaseUnplaced InstallPhase = iota
	PhaseSystemGroup
	PhasePlatformPool
)

func (p InstallPhase) String() string {
	switch p {
	case PhaseSystemGroup:
		return "system group (phase 1)"
	case PhasePlatformPool:
		return "platform pool (phase 2)"
	default:
		return "unplaced"
	}
}

// InstallPhaseOf is the phase of d, from the placement placeholders its
// platform values use. A definition that uses both ${system.*} and
// ${platform.*} is an error: one component runs on one pool.
func InstallPhaseOf(d Definition) (InstallPhase, error) {
	used := Placeholders(d.PlatformValues)
	system := slices.ContainsFunc(used, func(n string) bool { return strings.HasPrefix(n, "system.") })
	pool := slices.ContainsFunc(used, func(n string) bool { return strings.HasPrefix(n, "platform.") })
	switch {
	case system && pool:
		return PhaseUnplaced, fmt.Errorf("%s uses both ${system.*} and ${platform.*}: a component runs on the system group or on the platform pool, not both", d.Name)
	case system:
		return PhaseSystemGroup, nil
	case pool:
		return PhasePlatformPool, nil
	}
	return PhaseUnplaced, nil
}
