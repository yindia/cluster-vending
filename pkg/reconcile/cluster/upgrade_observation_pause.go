package cluster

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// ConditionUpgradePaused distinguishes an intentional reconciliation hold from
// an upgrade failure. The upgrade retains its normal phase and retry state.
const ConditionUpgradePaused = "UpgradePaused"

func upgradePauseReason(row *model.Cluster, upgrade *v1.ClusterUpgrade, views []*v1.ManagedResource) string {
	if upgrade == nil {
		return ""
	}
	if row.Frozen.Holds() {
		scope := row.Frozen.Scope
		if scope == "" {
			scope = "platform"
		}
		return scope + " is " + reconcile.FrozenBy(row.Frozen)
	}
	var paused []string
	for _, view := range views {
		if !view.GetPaused() {
			continue
		}
		relevant := false
		switch stepOf(upgrade) {
		case stepControlPlane:
			relevant = view.GetKind() == "Cluster"
		case stepNodeGroups:
			relevant = view.GetKind() == KindNodeGroup
		case stepAddonsPhase:
			relevant = view.GetKind() == "Addon"
		case stepPlatformComps, stepTenantComps:
			relevant = view.GetKind() == "Release"
		default:
			continue
		}
		if relevant {
			paused = append(paused, view.GetKind()+" "+view.GetName())
		}
	}
	if len(paused) == 0 {
		return ""
	}
	slices.Sort(paused)
	return strings.Join(paused, ", ") + " carries crossplane.io/paused"
}

func upgradePauseCondition(status *v1.ClusterStatus) *v1.Condition {
	for _, condition := range status.GetConditions() {
		if condition.GetType() == ConditionUpgradePaused && condition.GetStatus() == v1.ConditionStatus_CONDITION_STATUS_TRUE {
			return condition
		}
	}
	return nil
}

// Component Releases have their own inventory owners; include those only for
// the active components step, without adding them to the cluster's core inventory.
func (s *Reconciler) upgradePauseViews(ctx context.Context, row *model.Cluster, status *v1.ClusterStatus) ([]*v1.ManagedResource, error) {
	views := status.GetManagedResources()
	phase := stepOf(status.GetUpgrade())
	var records []model.ManagedResource
	switch phase {
	case stepPlatformComps:
		var err error
		records, err = s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerClusterComponents, row.ID)
		if err != nil {
			return nil, err
		}
	case stepTenantComps:
		components, err := s.deps.Store.Components().ListByCluster(ctx, row.TenantID, row.ID)
		if err != nil {
			return nil, err
		}
		for _, component := range components {
			if component.State == componentDeleting {
				continue
			}
			owned, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerComponent, component.ID)
			if err != nil {
				return nil, err
			}
			records = append(records, owned...)
		}
	default:
		return views, nil
	}
	observed, err := s.deps.Observe(ctx, records)
	if err != nil {
		return nil, err
	}
	return append(append([]*v1.ManagedResource{}, views...), observed...), nil
}

func withUpgradePause(row *model.Cluster, status *v1.ClusterStatus, views []*v1.ManagedResource) {
	upgrade := status.GetUpgrade()
	// An upgrade already halted on a failure is not stopped by this pause.
	// Keep its actionable failure visible and its retry requirement intact.
	if upgrade.GetHalted() {
		return
	}
	reason := upgradePauseReason(row, upgrade, views)
	if reason == "" {
		return
	}
	message := fmt.Sprintf("Kubernetes upgrade from %s to %s paused: %s; upgrade reconciliation resumes when the pause is lifted. Cloud operations already submitted may finish.", upgrade.FromVersion, upgrade.ToVersion, reason)
	upgrade.Message = message
	condition := &v1.Condition{Type: ConditionUpgradePaused, Status: v1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "ReconciliationPaused", Message: message, LastTransitionTime: timestamppb.New(now())}
	previous := storedStatus(row)
	if prior := upgradePauseCondition(previous); prior != nil && sameMinor(previous.GetUpgrade().GetToVersion(), upgrade.GetToVersion()) && prior.GetLastTransitionTime() != nil {
		condition.LastTransitionTime = prior.LastTransitionTime
	}
	status.Conditions = append(status.Conditions, condition)
	if status.GetState() != v1.ClusterState_CLUSTER_STATE_FAILED {
		status.State, status.Message = v1.ClusterState_CLUSTER_STATE_UPDATING, message
		for _, ready := range status.Conditions {
			if ready.GetType() == conditionReady {
				ready.Status, ready.Reason, ready.Message = v1.ConditionStatus_CONDITION_STATUS_FALSE, ConditionUpgradePaused, message
			}
		}
	} else {
		status.Message += "; " + message
	}
}

// Resume excludes time intentionally paused from the add-on recovery timeout.
// Keep the overall upgrade start, and never reset a failure's Halted flag.
func resumeUpgradeClock(row *model.Cluster, previous *v1.ClusterUpgrade) *v1.ClusterUpgrade {
	if stepOf(previous) != stepAddonsPhase {
		return previous
	}
	condition := upgradePauseCondition(storedStatus(row))
	if condition == nil || condition.GetLastTransitionTime() == nil || previous.GetPhaseStartTime() == nil {
		return previous
	}
	elapsed := max(time.Duration(0), now().Sub(condition.LastTransitionTime.AsTime()))
	resumed := proto.CloneOf(previous)
	resumed.PhaseStartTime = timestamppb.New(previous.PhaseStartTime.AsTime().Add(elapsed))
	return resumed
}
