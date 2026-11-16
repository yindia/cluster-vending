package admin

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/yindia/cluster-vending/pkg/convert"

	corev1 "k8s.io/api/core/v1"

	"github.com/yindia/cluster-vending/pkg/kube"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/equality"

	"connectrpc.com/connect/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func (s *Service) Freeze(ctx context.Context, req *cloudv1.FreezeRequest) (*cloudv1.FreezeResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	selected := convert.FreezeTarget(req.GetTarget(), req.GetTenantId(), req.GetNetworkId(), req.GetClusterId())
	target, err := s.freezeTarget(ctx, selected.GetTenantId(), selected.GetNetworkId(), selected.GetClusterId())
	if err != nil {
		return nil, err
	}
	current, err := s.hold(ctx, reconcile.FreezeName)
	if err != nil {
		return nil, err
	}
	scoped := target.TenantID != "" || target.NetworkID != "" || target.ClusterID != ""
	if current == nil {
		current = &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: reconcile.FreezeName}, Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Scoped: scoped}}
	}
	before := current.Spec.DeepCopy()
	reason := req.GetReason()
	if reason == "" {
		reason = "frozen by " + caller(ctx)
	}
	var until *metav1.Time
	if timestamp := req.GetUntil(); timestamp != nil {
		if !timestamp.AsTime().After(s.now()) {
			return nil, connect.NewError(connect.CodeInvalidArgument, "until has passed")
		}
		at := metav1.NewTime(timestamp.AsTime())
		until = &at
	}
	if !scoped {
		current.Spec.Platform, current.Spec.Scoped = true, false
		current.Spec.By, current.Spec.Reason, current.Spec.Until = caller(ctx), reason, until
		if req.GetResetExceptions() {
			current.Spec.Except, current.Spec.Rules = nil, nil
		}
	} else {
		if req.GetResetExceptions() {
			return nil, connect.NewError(connect.CodeInvalidArgument, "--reset-exceptions applies to a platform-wide freeze")
		}
		if current.Spec.Reason == "" {
			current.Spec.By, current.Spec.Reason = caller(ctx), reason
		}
		setFreezeRule(current, target, true, caller(ctx), reason, until, s.now())
	}
	if current.Spec.Revision == "" || !equality.Semantic.DeepEqual(*before, current.Spec) {
		current.Spec.Revision = uuid.NewString()
	}
	if err := s.writeHold(ctx, current); err != nil {
		return nil, err
	}
	status, err := s.GetFreeze(ctx, &cloudv1.GetFreezeRequest{Target: selected})
	if err != nil {
		return nil, err
	}
	if status.Revision != current.Spec.Revision {
		return nil, connect.NewError(connect.CodeAborted, "the freeze changed meanwhile; inspect admin freeze status")
	}
	return &cloudv1.FreezeResponse{Status: status.Status, Revision: status.Revision, Clusters: status.Clusters, Networks: status.Networks,
		PausedResources: status.PausedResources, TotalResources: status.TotalResources, Phase: status.Phase, Message: status.Message}, nil
}

func (s *Service) Unfreeze(ctx context.Context, req *cloudv1.UnfreezeRequest) (*cloudv1.UnfreezeResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}

	selected := convert.FreezeTarget(req.GetTarget(), req.GetTenantId(), req.GetNetworkId(), req.GetClusterId())
	current, err := s.hold(ctx, reconcile.FreezeName)
	if err != nil {
		return nil, err
	}
	if selected.GetPlatform() {
		if current != nil {
			if err := client.IgnoreNotFound(s.deps.Kube.Delete(ctx, current, client.Preconditions{UID: &current.UID, ResourceVersion: &current.ResourceVersion})); err != nil {
				return nil, s.freezeWriteError(ctx, err)
			}
		}
		if err := s.syncTenantFreezes(ctx); err != nil {
			return nil, err
		}
		return s.unfreezeResponse(ctx, selected, "", "unfreeze requested: all freeze scopes are cleared; the controller will resume pending changes")
	}
	if current == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "nothing is frozen")
	}
	target, err := s.freezeTarget(ctx, selected.GetTenantId(), selected.GetNetworkId(), selected.GetClusterId())
	if err != nil {
		return nil, err
	}
	targets, err := s.deps.FreezeTargets(ctx)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	current.Spec.Rules = slices.DeleteFunc(current.Spec.Rules, func(rule cloudv1alpha1.FreezeRule) bool {
		if !rule.Frozen {
			return false
		}
		nested := reconcile.FreezeTarget{TenantID: rule.TenantID, NetworkID: rule.NetworkID, ClusterID: rule.ClusterID}
		if known, ok := targets[rule.ClusterID]; ok {
			nested = known
		} else if known, ok := targets[rule.NetworkID]; ok {
			nested = known
		}
		return nested.Matches(cloudv1alpha1.FreezeRule{TenantID: target.TenantID, NetworkID: target.NetworkID, ClusterID: target.ClusterID})
	})
	setFreezeRule(current, target, false, caller(ctx), "", nil, s.now())
	current.Spec.Revision = uuid.NewString()
	if err := s.writeHold(ctx, current); err != nil {
		return nil, err
	}
	return s.unfreezeResponse(ctx, selected, current.Spec.Revision, fmt.Sprintf("unfreeze requested for %s; the controller will remove its freeze-owned pauses", freezeTargetText(target)))
}

func setFreezeRule(hold *cloudv1alpha1.ReleaseHold, target reconcile.FreezeTarget, frozen bool, by, reason string, until *metav1.Time, now time.Time) {
	hold.Spec.Rules = slices.DeleteFunc(hold.Spec.Rules, func(rule cloudv1alpha1.FreezeRule) bool {
		return rule.TenantID == target.TenantID && rule.NetworkID == target.NetworkID && rule.ClusterID == target.ClusterID
	})
	hold.Spec.Rules = append(hold.Spec.Rules, cloudv1alpha1.FreezeRule{TenantID: target.TenantID, NetworkID: target.NetworkID, ClusterID: target.ClusterID, Frozen: frozen, By: by, Reason: reason, Until: until, Since: metav1.NewTime(now)})
}

func (s *Service) unfreezeResponse(ctx context.Context, target *cloudv1.FreezeTarget, revision, note string) (*cloudv1.UnfreezeResponse, error) {
	view, err := s.GetFreeze(ctx, &cloudv1.GetFreezeRequest{Target: target})
	if err != nil {
		return nil, err
	}
	if view.GetRevision() != revision {
		return nil, connect.NewError(connect.CodeAborted, "the freeze changed meanwhile; inspect admin freeze status")
	}
	return &cloudv1.UnfreezeResponse{Note: note, Revision: revision, Status: view.GetStatus()}, nil
}

func freezeTargetText(target reconcile.FreezeTarget) string {
	switch {
	case target.ClusterID != "":
		return "cluster " + target.ClusterID
	case target.NetworkID != "":
		return "network " + target.NetworkID + " and its clusters"
	case target.TenantID != "":
		return "tenant " + target.TenantID
	default:
		return "the platform"
	}
}

func (s *Service) freezeTarget(ctx context.Context, tenantID, networkID, clusterID string) (reconcile.FreezeTarget, error) {
	switch {
	case clusterID != "":
		clusters, err := s.clusters(ctx)
		if err != nil {
			return reconcile.FreezeTarget{}, err
		}
		for _, cluster := range clusters {
			if cluster.Labels[cloudstore.LabelID] == clusterID {
				return reconcile.FreezeTarget{ClusterID: clusterID}, nil
			}
		}
		return reconcile.FreezeTarget{}, connect.NewError(connect.CodeNotFound, "cluster "+clusterID+" not found")
	case networkID != "":
		networks, err := s.networks(ctx)
		if err != nil {
			return reconcile.FreezeTarget{}, err
		}
		for _, network := range networks {
			if network.Labels[cloudstore.LabelID] == networkID {
				return reconcile.FreezeTarget{NetworkID: networkID}, nil
			}
		}
		return reconcile.FreezeTarget{}, connect.NewError(connect.CodeNotFound, "network "+networkID+" not found")
	case tenantID != "":
		namespace := &corev1.Namespace{}
		if err := s.reader().Get(ctx, client.ObjectKey{Name: kube.TenantNamespace(tenantID)}, namespace); err != nil {
			return reconcile.FreezeTarget{}, s.readError(ctx, "tenant "+tenantID, err)
		}
		return reconcile.FreezeTarget{TenantID: tenantID}, nil
	default:
		return reconcile.FreezeTarget{}, nil
	}
}

func (s *Service) hold(ctx context.Context, name string) (*cloudv1alpha1.ReleaseHold, error) {
	hold := &cloudv1alpha1.ReleaseHold{}
	err := s.reader().Get(ctx, client.ObjectKey{Name: name}, hold)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.readError(ctx, "freeze", err)
	}
	return hold, nil
}

func (s *Service) writeHold(ctx context.Context, hold *cloudv1alpha1.ReleaseHold) error {
	var err error
	if hold.ResourceVersion == "" {
		err = s.deps.Kube.Create(ctx, hold)
	} else {
		err = s.deps.Kube.Update(ctx, hold)
	}
	if err != nil {
		return s.freezeWriteError(ctx, err)
	}
	return s.syncTenantFreezes(ctx)
}

// ReleaseHold remains authoritative. Projection failures report that policy was
// saved so callers inspect status rather than assuming the mutation rolled back.
func (s *Service) syncTenantFreezes(ctx context.Context) error {
	d := reconcile.Deps{Kube: s.deps.Kube, KubeReader: s.reader()}
	if err := d.SyncTenantFreezes(ctx); err != nil {
		s.logger().ErrorContext(ctx, "project saved freeze policy onto tenants", "error", err)
		return connect.NewError(connect.CodeUnavailable, "freeze policy was saved, but tenant status could not be refreshed; inspect freeze status and retry")
	}
	return nil
}

func (s *Service) freezeWriteError(ctx context.Context, err error) error {
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return connect.NewError(connect.CodeAborted, "the freeze changed meanwhile; inspect admin freeze status and retry")
	}
	s.logger().ErrorContext(ctx, "change the platform freeze", "error", err)
	return connect.NewError(connect.CodeUnavailable, "cannot change the platform freeze; inspect admin freeze status and retry")
}
