package cluster

import (
	"context"
	"slices"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// componentsOf is how the components stand for an upgrade's last two steps:
// the platform's as this pass installed them (installed; nil: as recorded
// with the row), the tenant's as their statuses say. A component that is
// not READY is behind; a cluster with none is settled.
func (s *Reconciler) componentsOf(ctx context.Context, row *model.Cluster, installed *platform) componentsState {
	if installed == nil {
		installed = storedPlatform(row)
	}
	var cs componentsState
	cs.platformBehind = notReady(installed.componentsOrNil())
	cs.platformReady = len(cs.platformBehind) == 0
	cs.platformUnchanged = allUnchanged(installed.componentsOrNil())
	if s.deps.Store == nil {
		cs.tenantReady = true
		return cs
	}
	rows, err := s.deps.Store.Components().ListByCluster(ctx, row.TenantID, row.ID)
	if err != nil {
		// Unknown: not settled, so an upgrade does not finish on a failed read.
		return cs
	}
	cs.tenantUnchanged = len(rows) > 0
	for _, c := range rows {
		if c.State == componentDeleting {
			continue
		}
		cs.tenantUnchanged = cs.tenantUnchanged && tenantStatus(c).GetUnchanged()
		if tenantStatus(c).GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			cs.tenantBehind = append(cs.tenantBehind, c.Component)
		}
	}
	cs.tenantReady = len(cs.tenantBehind) == 0
	return cs
}

// allUnchanged reports whether there are components and none was written
// in the pass that found them.
func allUnchanged(cs []*cloudv1.PlatformComponent) bool {
	return len(cs) > 0 && !slices.ContainsFunc(cs, func(c *cloudv1.PlatformComponent) bool { return !c.GetUnchanged() })
}

// componentsOrNil are p's components; none for a nil p.
func (p *platform) componentsOrNil() []*cloudv1.PlatformComponent {
	if p == nil {
		return nil
	}
	return p.components
}

// notReady are the names of the components that are not READY.
func notReady(cs []*cloudv1.PlatformComponent) []string {
	var out []string
	for _, c := range cs {
		if c.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			out = append(out, c.GetName())
		}
	}
	return out
}
