package admin

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/rollout"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// freezeSpecOf is the freeze a request names: a tenant's, a channel's
// (one tier's), or with neither the platform's.
func freezeSpecOf(tenant string, channel cloudv1.ReleaseChannel, tier cloudv1.ClusterTier) cloudv1alpha1.ReleaseHoldSpec {
	t := pauseTargetOf(tenant, "", "", channel, tier)
	if t.tenant == "" && t.channel == "" {
		return cloudv1alpha1.ReleaseHoldSpec{Platform: true}
	}
	return t.holdSpec()
}

// Freeze freezes the platform, a tenant or a channel: one release hold per
// target (rollout.FreezeNameOf), which the rollouts and the reconcilers
// read: no automatic change of what it covers starts while it exists.
// Freezing again changes why, until when and by whom, and keeps what was
// let through.
func (s *Service) Freeze(ctx context.Context, req *cloudv1.FreezeRequest) (*cloudv1.FreezeResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	spec := freezeSpecOf(req.GetTenantId(), req.GetChannel(), req.GetTier())
	spec.Reason, spec.By = req.GetReason(), caller(ctx)
	if spec.Reason == "" {
		spec.Reason = "frozen by " + spec.By // a hold needs a reason
	}
	if u := req.GetUntil(); u != nil {
		until := u.AsTime().UTC()
		if !until.After(s.now()) {
			return nil, connect.NewError(connect.CodeInvalidArgument, "until "+until.Format(time.RFC3339)+" has passed")
		}
		at := metav1.NewTime(until)
		spec.Until = &at
	}
	if spec.TenantID != "" {
		if _, err := s.deps.Store.Tenants().Get(ctx, spec.TenantID); err != nil {
			return nil, s.storeError(ctx, "freeze", spec.TenantID, pauseTarget{}.rows(), err)
		}
	}
	name := rollout.FreezeNameOf(spec)
	current, err := s.hold(ctx, name)
	if err != nil {
		return nil, err
	}
	if current != nil {
		spec.Except = current.Spec.Except
	}
	if err := s.writeHold(ctx, &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}); err != nil {
		return nil, err
	}
	all, err := s.clusters(ctx)
	if err != nil {
		return nil, err
	}
	s.logger().InfoContext(ctx, "frozen", "target", rollout.HoldTarget(spec), "by", spec.By, "reason", spec.Reason)
	return &cloudv1.FreezeResponse{Clusters: count(matching(all, spec))}, nil
}

// Unfreeze lifts a freeze, or lets one cluster or network through it.
func (s *Service) Unfreeze(ctx context.Context, req *cloudv1.UnfreezeRequest) (*cloudv1.UnfreezeResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	spec := freezeSpecOf(req.GetTenantId(), req.GetChannel(), req.GetTier())
	target := rollout.HoldTarget(spec)
	current, err := s.hold(ctx, rollout.FreezeNameOf(spec))
	if err != nil {
		return nil, err
	}
	id, what := req.GetClusterId(), "cluster"
	if id == "" {
		id, what = req.GetNetworkId(), "network"
	}
	if id == "" {
		if current == nil {
			return &cloudv1.UnfreezeResponse{Note: target + " is not frozen"}, nil
		}
		if err := client.IgnoreNotFound(s.deps.Kube.Delete(ctx, current)); err != nil {
			s.logger().ErrorContext(ctx, "lift the freeze", "target", target, "error", err)
			return nil, connect.NewError(connect.CodeUnavailable, "cannot lift the freeze; try again")
		}
		s.logger().InfoContext(ctx, "unfrozen", "target", target, "by", caller(ctx))
		return &cloudv1.UnfreezeResponse{Note: target + " is not frozen any more: automatic changes start again, tier by tier"}, nil
	}
	if current == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, target+" is not frozen: nothing to let "+what+" "+id+" through")
	}
	if !slices.Contains(current.Spec.Except, id) {
		current.Spec.Except = append(slices.Clone(current.Spec.Except), id)
		if err := s.deps.Kube.Update(ctx, current); err != nil {
			s.logger().ErrorContext(ctx, "let through the freeze", "target", target, what, id, "error", err)
			return nil, connect.NewError(connect.CodeUnavailable, "cannot let "+what+" "+id+" through; try again")
		}
	}
	return &cloudv1.UnfreezeResponse{Note: fmt.Sprintf("%s %s is let through: its pending changes start; %s stays frozen (%d let through)",
		what, id, target, len(current.Spec.Except))}, nil
}

// hold is the release hold of that name; nil when there is none.
func (s *Service) hold(ctx context.Context, name string) (*cloudv1alpha1.ReleaseHold, error) {
	h := &cloudv1alpha1.ReleaseHold{}
	err := s.deps.Kube.Get(ctx, client.ObjectKey{Name: name}, h)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		s.logger().ErrorContext(ctx, "read the freeze", "name", name, "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the freeze; try again")
	}
	return h, nil
}

// writeHold creates the hold, or sets the spec of the one of that name.
func (s *Service) writeHold(ctx context.Context, hold *cloudv1alpha1.ReleaseHold) error {
	err := s.deps.Kube.Create(ctx, hold)
	if apierrors.IsAlreadyExists(err) {
		current := &cloudv1alpha1.ReleaseHold{}
		if err = s.deps.Kube.Get(ctx, client.ObjectKeyFromObject(hold), current); err == nil {
			current.Spec = hold.Spec
			err = s.deps.Kube.Update(ctx, current)
		}
	}
	if err != nil {
		s.logger().ErrorContext(ctx, "write the freeze", "name", hold.Name, "error", err)
		return connect.NewError(connect.CodeUnavailable, "cannot record the freeze; try again")
	}
	return nil
}

// freezeRecord is a release hold as ListPauses shows it: a freeze, which
// stops automatic changes.
func freezeRecord(h *cloudv1alpha1.ReleaseHold, clusters int32) *cloudv1.PauseRecord {
	r := &cloudv1.PauseRecord{Target: rollout.HoldTarget(h.Spec), TenantId: h.Spec.TenantID, ClusterId: h.Spec.ClusterID,
		Channel: channelEnum(h.Spec.Channel), Kind: cloudv1.PauseKind_PAUSE_KIND_AUTOMATIC_CHANGES, By: h.Spec.By,
		Since: timestamppb.New(h.CreationTimestamp.Time), Reason: h.Spec.Reason, Clusters: clusters}
	if h.Spec.Tier != "" {
		r.Tier = tierEnum(strings.TrimPrefix(string(h.Spec.Tier), "Tier"))
	}
	if h.Spec.Until != nil {
		r.Until = timestamppb.New(h.Spec.Until.Time)
	}
	return r
}
