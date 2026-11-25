package controller

import (
	"context"
	"fmt"
	"strings"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
)

// Recorded component progress is only a hint: the caller checks fresh runtime
// availability before treating a non-Ready Capsule phase as a blocker. The
// parent's aggregate phase includes unrelated infrastructure and upgrades.
// tenancy names the catalog's NamespaceTenancy definition (tenancyName).
func subTenantPrerequisite(parent *cloud.Cluster, tenancy string) (reason, message string, failed bool) {
	for _, pc := range parent.Status.PlatformComponents {
		// Only the actual namespace tenancy component satisfies this prerequisite.
		if pc.Name != tenancy {
			continue
		}
		if strings.EqualFold(pc.Phase, "Ready") {
			return "", "", false
		}
		detail := pc.Message
		if detail == "" {
			detail = "installation or upgrade is in progress"
		}
		if strings.EqualFold(pc.Phase, "Failed") {
			return "CapsuleFailed", fmt.Sprintf("Waiting for a serving Capsule controller and webhook on cluster %s: %s", parent.Name, detail), true
		}
		return "CapsulePending", fmt.Sprintf("Waiting for a serving Capsule controller and webhook on cluster %s (recorded %s): %s", parent.Name, pc.Phase, detail), false
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

// tenancyName is the name of the catalog's NamespaceTenancy definition
// (component.RoleNamespaceTenancy): the component sub-tenants wait for. The
// shipped one's when the catalog cannot be read.
func (r *SubTenantReconciler) tenancyName(ctx context.Context) string {
	defs := &cloud.ComponentDefinitionList{}
	if err := r.deps.listCached(ctx, defs); err != nil {
		r.deps.logger().WarnContext(ctx, "sub-tenant: the component catalog cannot be read; namespace tenancy is taken to be the shipped definition", "error", err)
		return component.RoleName(nil, component.RoleNamespaceTenancy, "")
	}
	return component.RoleNameOf(defs.Items, component.RoleNamespaceTenancy, "")
}
