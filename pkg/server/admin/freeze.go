package admin

import (
	"context"
	"fmt"
	"slices"

	"connectrpc.com/connect/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/rollout"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// Freeze freezes the platform: one release hold of every cluster and
// network (rollout.FreezeName), which the rollouts and the reconcilers
// read: no automatic change starts while it exists. Freezing again
// changes why and by whom, and keeps what was let through.
func (s *Service) Freeze(ctx context.Context, req *cloudv1.FreezeRequest) (*cloudv1.FreezeResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	spec := cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: req.GetReason(), By: caller(ctx)}
	if spec.Reason == "" {
		spec.Reason = "frozen by " + spec.By // a hold needs a reason
	}
	current, err := s.freeze(ctx)
	if err != nil {
		return nil, err
	}
	if current != nil {
		spec.Except = current.Spec.Except
	}
	if err := s.writeHold(ctx, &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: rollout.FreezeName}, Spec: spec}); err != nil {
		return nil, err
	}
	all, err := s.clusters(ctx)
	if err != nil {
		return nil, err
	}
	s.logger().InfoContext(ctx, "platform frozen", "by", spec.By, "reason", spec.Reason)
	return &cloudv1.FreezeResponse{Clusters: count(matching(all, spec))}, nil
}

// Unfreeze lifts the freeze, or lets one cluster or network through it.
func (s *Service) Unfreeze(ctx context.Context, req *cloudv1.UnfreezeRequest) (*cloudv1.UnfreezeResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	current, err := s.freeze(ctx)
	if err != nil {
		return nil, err
	}
	id, what := req.GetClusterId(), "cluster"
	if id == "" {
		id, what = req.GetNetworkId(), "network"
	}
	if id == "" {
		if current == nil {
			return &cloudv1.UnfreezeResponse{Note: "the platform is not frozen"}, nil
		}
		if err := client.IgnoreNotFound(s.deps.Kube.Delete(ctx, current)); err != nil {
			s.logger().ErrorContext(ctx, "lift the platform freeze", "error", err)
			return nil, connect.NewError(connect.CodeUnavailable, "cannot lift the freeze; try again")
		}
		s.logger().InfoContext(ctx, "platform unfrozen", "by", caller(ctx))
		return &cloudv1.UnfreezeResponse{Note: "the platform is not frozen any more: automatic changes start again, tier by tier"}, nil
	}
	if current == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "the platform is not frozen: nothing to let "+what+" "+id+" through")
	}
	if !slices.Contains(current.Spec.Except, id) {
		current.Spec.Except = append(current.Spec.Except, id)
		if err := s.deps.Kube.Update(ctx, current); err != nil {
			s.logger().ErrorContext(ctx, "let through the platform freeze", what, id, "error", err)
			return nil, connect.NewError(connect.CodeUnavailable, "cannot let "+what+" "+id+" through; try again")
		}
	}
	return &cloudv1.UnfreezeResponse{Note: fmt.Sprintf("%s %s is let through: its pending changes start; the platform stays frozen (%d let through)",
		what, id, len(current.Spec.Except))}, nil
}

// freeze is the platform freeze; nil when the platform is not frozen.
func (s *Service) freeze(ctx context.Context) (*cloudv1alpha1.ReleaseHold, error) {
	h := &cloudv1alpha1.ReleaseHold{}
	err := s.deps.Kube.Get(ctx, client.ObjectKey{Name: rollout.FreezeName}, h)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		s.logger().ErrorContext(ctx, "read the platform freeze", "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the platform freeze; try again")
	}
	return h, nil
}
