// Package admin implements the cloud.v1 AdminService: operations of the
// platform's operators on tenants' managed resources.
package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// PausedAnnotation makes Crossplane providers skip a managed resource.
const PausedAnnotation = managed.PausedAnnotation

// Service implements cloud.v1.AdminService.
type Service struct {
	deps reconcile.Deps
	// now is the clock; tests replace it.
	now func() time.Time
}

var _ cloudv1connect.AdminServiceHandler = (*Service)(nil)

// NewService returns the AdminService.
func NewService(d reconcile.Deps) *Service { return &Service{deps: d, now: time.Now} }

// PauseReconcile pauses the tenant, or one of its clusters. The pause is
// recorded on the rows first (the tenant's accounts, networks and
// clusters, or the one cluster: Tenants.SetPause), with who asked, when
// and why, so the controller stops acting on them; then every managed
// resource of the scope gets the providers' pause.
func (s *Service) PauseReconcile(ctx context.Context, req *cloudv1.PauseReconcileRequest) (*cloudv1.PauseReconcileResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	target := model.PauseTarget{NetworkID: req.GetNetworkId(), ClusterID: req.GetClusterId()}
	pause := &model.Pause{By: caller(ctx), Since: s.now().UTC(), Reason: req.GetReason()}
	if err := s.deps.Store.Tenants().SetPause(ctx, req.GetTenantId(), target, pause); err != nil {
		return nil, s.storeError(ctx, "pause", req.GetTenantId(), target, err)
	}
	n, err := s.setPaused(ctx, req.GetTenantId(), target, true)
	return &cloudv1.PauseReconcileResponse{Resources: n}, err
}

// ResumeReconcile resumes the tenant, or one of its clusters: the pause
// recorded on the rows is removed, then the providers' pause on the
// managed resources. The controller's next pass observes each resource
// and applies what was asked for meanwhile.
func (s *Service) ResumeReconcile(ctx context.Context, req *cloudv1.ResumeReconcileRequest) (*cloudv1.ResumeReconcileResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	target := model.PauseTarget{NetworkID: req.GetNetworkId(), ClusterID: req.GetClusterId()}
	if err := s.deps.Store.Tenants().SetPause(ctx, req.GetTenantId(), target, nil); err != nil {
		return nil, s.storeError(ctx, "resume", req.GetTenantId(), target, err)
	}
	n, err := s.setPaused(ctx, req.GetTenantId(), target, false)
	return &cloudv1.ResumeReconcileResponse{Resources: n}, err
}

// anonymousCaller is who paused when the API runs without authentication.
const anonymousCaller = "anonymous"

// caller names who makes the request, for the pause's record: the
// caller's username, else its subject; "anonymous" without a token.
func caller(ctx context.Context) string {
	id := auth.FromContext(ctx)
	switch {
	case id == nil || id.Anonymous:
		return anonymousCaller
	case id.Username != "":
		return id.Username
	case id.Subject != "":
		return id.Subject
	default:
		return anonymousCaller
	}
}

// storeError answers a failed record of a pause: NOT_FOUND for a tenant or
// a cluster that does not exist, else what the store's failures map to.
func (s *Service) storeError(ctx context.Context, verb, tenantID string, target model.PauseTarget, err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		switch {
		case target.ClusterID != "":
			return connect.NewError(connect.CodeNotFound, fmt.Sprintf("cluster %s of tenant %s not found", target.ClusterID, tenantID))
		case target.NetworkID != "":
			return connect.NewError(connect.CodeNotFound, fmt.Sprintf("network %s of tenant %s not found", target.NetworkID, tenantID))
		}
		return connect.NewError(connect.CodeNotFound, fmt.Sprintf("tenant %s not found", tenantID))
	}
	s.logger().ErrorContext(ctx, "record the "+verb, "tenant_id", tenantID, "cluster_id", target.ClusterID,
		"network_id", target.NetworkID, "error", err)
	return service.ToConnect(err)
}

// setPaused sets or removes the providers' pause on every managed
// resource of the target (reconcile.SetProvidersPaused).
func (s *Service) setPaused(ctx context.Context, tenantID string, target model.PauseTarget, paused bool) (int32, error) {
	n, err := reconcile.SetProvidersPaused(ctx, s.deps, tenantID, target, paused)
	return n, service.ToConnect(err)
}

func (s *Service) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}
