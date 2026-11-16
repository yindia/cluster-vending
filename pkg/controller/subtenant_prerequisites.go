package controller

import (
	"fmt"
	"strings"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

// Recorded component progress is only a hint: the caller checks fresh runtime
// availability before treating a non-Ready Capsule phase as a blocker. The
// parent's aggregate phase includes unrelated infrastructure and upgrades.
func subTenantPrerequisite(parent *cloud.Cluster) (reason, message string, failed bool) {
	for _, component := range parent.Status.PlatformComponents {
		// Only the actual Capsule component satisfies this prerequisite.
		if component.Name != "capsule" {
			continue
		}
		if strings.EqualFold(component.Phase, "Ready") {
			return "", "", false
		}
		detail := component.Message
		if detail == "" {
			detail = "installation or upgrade is in progress"
		}
		if strings.EqualFold(component.Phase, "Failed") {
			return "CapsuleFailed", fmt.Sprintf("Waiting for a serving Capsule controller and webhook on cluster %s: %s", parent.Name, detail), true
		}
		return "CapsulePending", fmt.Sprintf("Waiting for a serving Capsule controller and webhook on cluster %s (recorded %s): %s", parent.Name, component.Phase, detail), false
	}
	return "CapsulePending", fmt.Sprintf("Waiting for a serving Capsule installation on cluster %s; Sub Tenant provisioning will resume automatically", parent.Name), false
}

func subTenantPrerequisiteReason(reason string) bool {
	switch reason {
	case "ParentBindingUnavailable", "FreezeObservationUnavailable", "ReconciliationPaused", "CapsulePending", "CapsuleFailed", "BootstrapPending", "WorkloadClientPending", "WorkloadClientUnavailable", "CapsuleAPIUnavailable":
		return true
	default:
		return false
	}
}
