package cluster

import (
	"fmt"
	"strings"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// tenantComponentsReadyNote is the cluster's TenantComponentsReady event:
// told when, between two reads of its components (before and after a
// pass), the last enabled tenant component became Ready. Components being
// removed do not count.
func tenantComponentsReadyNote(before, after []model.Component) (reconcile.Note, bool) {
	if allTenantReady(before) {
		return reconcile.Note{}, false
	}
	if !allTenantReady(after) {
		return reconcile.Note{}, false
	}
	names := enabledTenant(after)
	return reconcile.Normal(reconcile.ReasonTenantComponentsReady,
		fmt.Sprintf("%d tenant component(s) installed: %s", len(names), strings.Join(names, ", "))), true
}

// allTenantReady reports whether the cluster has enabled tenant components
// and every one is Ready.
func allTenantReady(cs []model.Component) bool {
	n := 0
	for _, c := range cs {
		if c.State == componentDeleting {
			continue
		}
		if tenantStatus(c).GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			return false
		}
		n++
	}
	return n > 0
}

func enabledTenant(cs []model.Component) []string {
	var out []string
	for _, c := range cs {
		if c.State != componentDeleting {
			out = append(out, c.Component)
		}
	}
	return out
}
