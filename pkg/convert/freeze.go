package convert

import (
	"strings"

	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// FreezeTarget normalizes legacy request fields into the shared target.
// Matching legacy fields may accompany target for older server compatibility.
func FreezeTarget(target *v1.FreezeTarget, tenant, network, cluster string) *v1.FreezeTarget {
	if target != nil {
		return target
	}
	switch {
	case cluster != "":
		return &v1.FreezeTarget{Scope: &v1.FreezeTarget_ClusterId{ClusterId: cluster}}
	case network != "":
		return &v1.FreezeTarget{Scope: &v1.FreezeTarget_NetworkId{NetworkId: network}}
	case tenant != "":
		return &v1.FreezeTarget{Scope: &v1.FreezeTarget_TenantId{TenantId: tenant}}
	default:
		return &v1.FreezeTarget{Scope: &v1.FreezeTarget_Platform{Platform: true}}
	}
}

func FreezePhase(phase string) v1.FreezePhase {
	return v1.FreezePhase(v1.FreezePhase_value["FREEZE_PHASE_"+strings.ToUpper(phase)])
}

func FreezePhaseText(phase v1.FreezePhase) string {
	switch phase {
	case v1.FreezePhase_FREEZE_PHASE_FREEZING:
		return "Freezing"
	case v1.FreezePhase_FREEZE_PHASE_FROZEN:
		return "Frozen"
	case v1.FreezePhase_FREEZE_PHASE_UNFREEZING:
		return "Unfreezing"
	case v1.FreezePhase_FREEZE_PHASE_UNFROZEN:
		return "Unfrozen"
	default:
		return "Unknown"
	}
}

// FreezeStatus preserves the legacy view while exposing one canonical contract.
func FreezeStatus(view *v1.GetFreezeResponse, target *v1.FreezeTarget) *v1.FreezeStatus {
	if view.GetStatus() != nil {
		return view.GetStatus()
	}
	return &v1.FreezeStatus{Target: target, Phase: FreezePhase(view.GetPhase()), Revision: view.GetRevision(),
		Message: view.GetMessage(), Freeze: view.GetFreeze(), Clusters: view.GetClusters(), Networks: view.GetNetworks(),
		TotalResources: view.GetTotalResources(), PausedResources: view.GetPausedResources(), NewlyPausedResources: view.GetNewlyPausedResources(),
		PlatformFrozen: view.GetPlatformFrozen(), Scopes: view.GetScopes(), Exceptions: view.GetExceptions()}
}
