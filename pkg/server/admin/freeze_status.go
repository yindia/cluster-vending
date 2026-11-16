package admin

import (
	"context"
	"slices"

	"github.com/yindia/cluster-vending/pkg/convert"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func (s *Service) GetFreeze(ctx context.Context, req *cloudv1.GetFreezeRequest) (*cloudv1.GetFreezeResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}

	target := convert.FreezeTarget(req.GetTarget(), req.GetTenantId(), req.GetNetworkId(), req.GetClusterId())
	view, err := s.freezeView(ctx, &cloudv1.GetFreezeRequest{TenantId: target.GetTenantId(), NetworkId: target.GetNetworkId(), ClusterId: target.GetClusterId()})
	if err != nil {
		return nil, err
	}
	view.Status = convert.FreezeStatus(view, target)
	return view, nil
}

func (s *Service) freezeView(ctx context.Context, req *cloudv1.GetFreezeRequest) (*cloudv1.GetFreezeResponse, error) {
	hold, err := s.hold(ctx, reconcile.FreezeName)
	if err != nil {
		return nil, err
	}
	if req.GetClusterId() != "" || req.GetNetworkId() != "" || req.GetTenantId() != "" {
		return s.targetFreezeStatus(ctx, req, hold)
	}
	active := reconcile.AnyFreezeActive(hold, s.now())
	targets, err := s.deps.FreezeTargets(ctx)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	scope := reconcile.HeldFreezeScope(hold, targets, s.now())
	result := &cloudv1.GetFreezeResponse{Phase: "Unfrozen", PlatformFrozen: reconcile.GlobalFreezeActive(hold, s.now())}
	if hold != nil {
		result.Revision = hold.Spec.Revision
		if active {
			frozen := &model.Freeze{Scope: "platform", By: hold.Spec.By, Reason: hold.Spec.Reason, Since: hold.CreationTimestamp.Time}
			if result.PlatformFrozen && hold.Spec.Until != nil {
				until := hold.Spec.Until.Time
				frozen.Until = &until
			}
			if !result.PlatformFrozen {
				frozen.Scope = "scoped resources"
			}
			result.Freeze = reconcile.FreezeProto(frozen)
		}
		result.Exceptions = slices.Clone(hold.Spec.Except)
		for _, rule := range hold.Spec.Rules {
			if !reconcile.RuleActive(rule, s.now()) {
				continue
			}
			entry := &cloudv1.FreezeScope{TenantId: rule.TenantID, NetworkId: rule.NetworkID, ClusterId: rule.ClusterID, Frozen: rule.Frozen, Reason: rule.Reason}
			if rule.Until != nil {
				entry.Until = timestamppb.New(rule.Until.Time)
			}
			result.Scopes = append(result.Scopes, entry)
			if !rule.Frozen {
				result.Exceptions = append(result.Exceptions, freezeTargetText(reconcile.FreezeTarget{TenantID: rule.TenantID, NetworkID: rule.NetworkID, ClusterID: rule.ClusterID}))
			}
		}
	}

	counts, err := reconcile.ProviderPauseCounts(ctx, s.deps, scope)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	active = reconcile.EffectiveFreezeActive(hold, targets, s.now()) || counts.Total > 0
	if !active {
		counts, err = reconcile.ProviderPauseCounts(ctx, s.deps, reconcile.PauseScope{})
		if err != nil {
			return nil, service.ToConnect(err)
		}
	}
	result.TotalResources, result.PausedResources = counts.Total, counts.Paused
	if !active {
		result.Freeze = nil
		if counts.Owned > 0 {
			result.Phase, result.Message = "Unfreezing", "waiting for the controller to remove freeze-owned provider pauses"
		}
		return result, nil
	}
	result.Phase, result.Message = "Freezing", "waiting for the controller to drain active reconciliations and confirm provider pauses"
	if hold.Status.FreezeRevision == hold.Spec.Revision && hold.Status.ObservedGeneration == hold.Generation && hold.Status.FreezePhase == "Frozen" && counts.Total == counts.Paused {
		result.Phase, result.Message = "Frozen", "controller reconciliations drained; held managed resources paused"
		result.NewlyPausedResources = hold.Status.NewlyPausedResources
	}
	clusters, err := s.clusters(ctx)
	if err != nil {
		return nil, err
	}
	networks, err := s.networks(ctx)
	if err != nil {
		return nil, err
	}
	for _, cluster := range clusters {
		if reconcile.FreezeForTarget(hold, s.now(), targets[cluster.Labels[cloudstore.LabelID]]).Holds() {
			result.Clusters++
		}
	}
	for _, network := range networks {
		if reconcile.FreezeForTarget(hold, s.now(), targets[network.Labels[cloudstore.LabelID]]).Holds() {
			result.Networks++
		}
	}
	return result, nil
}
