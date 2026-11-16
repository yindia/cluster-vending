package admin

import (
	"context"
	"fmt"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func (s *Service) targetFreezeStatus(ctx context.Context, req *v1.GetFreezeRequest, hold *cloud.ReleaseHold) (*v1.GetFreezeResponse, error) {
	target, err := s.freezeTarget(ctx, req.GetTenantId(), req.GetNetworkId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	selected := cloud.FreezeRule{TenantID: target.TenantID, NetworkID: target.NetworkID, ClusterID: target.ClusterID}
	target, err = s.deps.ResolveFreezeTarget(ctx, target)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	targets, err := s.deps.FreezeTargets(ctx)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	scope := reconcile.PauseScope{Targets: targets, Select: func(resource reconcile.FreezeTarget) bool { return resource.Matches(selected) }}
	counts, err := reconcile.ProviderPauseCounts(ctx, s.deps, scope)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	now := s.now()
	frozen := reconcile.FreezeForTarget(hold, now, target)
	held := frozen.Holds()
	result := &v1.GetFreezeResponse{Phase: "Unfrozen", TotalResources: counts.Total, PausedResources: counts.Paused,
		PlatformFrozen: reconcile.GlobalFreezeActive(hold, now)}
	if hold != nil {
		result.Revision = hold.Spec.Revision
	}
	for _, resource := range targets {
		if !resource.Matches(selected) {
			continue
		}
		if reconcile.FreezeForTarget(hold, now, resource).Holds() {
			held = true
			if resource.ClusterID != "" {
				result.Clusters++
			} else if resource.NetworkID != "" {
				result.Networks++
			}
		}
	}
	confirmed := hold == nil || (hold.Status.DrainedRevision == hold.Spec.Revision && hold.Status.DrainedGeneration == hold.Generation) || (hold.Status.FreezeRevision == hold.Spec.Revision && hold.Status.ObservedGeneration == hold.Generation)
	switch {
	case held:
		result.Freeze = reconcile.FreezeProto(frozen)
		result.Phase = "Freezing"
		heldScope := scope
		heldScope.Select = func(resource reconcile.FreezeTarget) bool {
			return resource.Matches(selected) && reconcile.FreezeForTarget(hold, now, resource).Holds()
		}
		heldCounts, err := reconcile.ProviderPauseCounts(ctx, s.deps, heldScope)
		if err != nil {
			return nil, service.ToConnect(err)
		}
		if confirmed && heldCounts.Total == heldCounts.Paused {
			result.Phase = "Frozen"
		}
		result.Message = fmt.Sprintf("%s includes frozen resources; unfreeze this scope to resume all of them", freezeTargetText(reconcile.FreezeTarget{TenantID: selected.TenantID, NetworkID: selected.NetworkID, ClusterID: selected.ClusterID}))
	case counts.Owned > 0 || !confirmed:
		result.Phase = "Unfreezing"
		result.Message = fmt.Sprintf("waiting for controller confirmation; %d freeze-owned provider pauses remain", counts.Owned)
	default:
		result.Message = "controller confirmed; freeze-owned pauses removed; manual pauses and normal rollout gates still apply"
	}
	return result, nil
}
