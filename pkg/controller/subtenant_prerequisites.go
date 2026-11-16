package controller

import (
	"fmt"
	"strings"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

// Progress is an asynchronous prerequisite, not a rejected allocation. The
// caller persists it and requeues until Capsule is ready. The parent's coarse
// phase and aggregate Ready condition include unrelated infrastructure and
// upgrades; they must not prevent use of an available Capsule installation.
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
			return "CapsuleFailed", fmt.Sprintf("Waiting for Capsule on cluster %s to recover: %s", parent.Name, detail), true
		}
		return "CapsulePending", fmt.Sprintf("Waiting for Capsule on cluster %s to become Ready (%s): %s", parent.Name, component.Phase, detail), false
	}
	return "CapsulePending", fmt.Sprintf("Waiting for Capsule installation to be reported on cluster %s; Sub Tenant provisioning will resume automatically", parent.Name), false
}

func subTenantPrerequisiteReason(reason string) bool {
	switch reason {
	case "ParentBindingUnavailable", "FreezeObservationUnavailable", "ReconciliationPaused", "CapsulePending", "CapsuleFailed", "BootstrapPending", "WorkloadClientPending", "WorkloadClientUnavailable", "CapsuleAPIUnavailable":
		return true
	default:
		return false
	}
}
