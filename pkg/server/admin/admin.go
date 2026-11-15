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

// PauseReconcile pauses a tenant, one of its networks or clusters, or a
// release channel's clusters (one tier, or all): the rows and their
// managed resources (pauseRows, pauseChannel). AdminService.Freeze stops
// automatic changes only.
func (s *Service) PauseReconcile(ctx context.Context, req *cloudv1.PauseReconcileRequest) (*cloudv1.PauseReconcileResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	pause := &model.Pause{By: caller(ctx), Since: s.now().UTC(), Reason: req.GetReason()}
	if u := req.GetUntil(); u != nil {
		until := u.AsTime().UTC()
		if !until.After(s.now()) {
			return nil, connect.NewError(connect.CodeInvalidArgument, "until "+until.Format(time.RFC3339)+" has passed")
		}
		pause.Until = &until
	}
	t := pauseTargetOf(req.GetTenantId(), req.GetClusterId(), req.GetNetworkId(), req.GetChannel(), req.GetTier())
	if t.channel != "" {
		return s.pauseChannel(ctx, t, pause)
	}
	return s.pauseRows(ctx, t, pause)
}

// ResumeReconcile ends what PauseReconcile of the same target asked for:
// a full pause (resumeRows, resumeChannel).
func (s *Service) ResumeReconcile(ctx context.Context, req *cloudv1.ResumeReconcileRequest) (*cloudv1.ResumeReconcileResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	t := pauseTargetOf(req.GetTenantId(), req.GetClusterId(), req.GetNetworkId(), req.GetChannel(), req.GetTier())
	if t.channel != "" {
		return s.resumeChannel(ctx, t)
	}
	return s.resumeRows(ctx, t)
}

// pauseRows pauses the tenant, or one of its networks or clusters. The
// pause is recorded on the rows first (the tenant's accounts, networks
// and clusters, or the one: Tenants.SetPause), with who asked, when, why
// and until when, so the controller stops acting on them; then every
// managed resource of the scope gets the providers' pause.
func (s *Service) pauseRows(ctx context.Context, t pauseTarget, pause *model.Pause) (*cloudv1.PauseReconcileResponse, error) {
	target := t.rows()
	if err := s.deps.Store.Tenants().SetPause(ctx, t.tenant, target, pause); err != nil {
		return nil, s.storeError(ctx, "pause", t.tenant, target, err)
	}
	n, err := s.setPaused(ctx, t.tenant, target, true)
	return &cloudv1.PauseReconcileResponse{Resources: n, Clusters: s.clustersOf(ctx, t)}, err
}

// resumeRows resumes the tenant, or one of its networks or clusters: the
// pause recorded on the rows is removed, then the providers' pause on the
// managed resources. The controller's next pass observes each resource
// and applies what was asked for meanwhile.
func (s *Service) resumeRows(ctx context.Context, t pauseTarget) (*cloudv1.ResumeReconcileResponse, error) {
	target := t.rows()
	if err := s.deps.Store.Tenants().SetPause(ctx, t.tenant, target, nil); err != nil {
		return nil, s.storeError(ctx, "resume", t.tenant, target, err)
	}
	n, err := s.setPaused(ctx, t.tenant, target, false)
	return &cloudv1.ResumeReconcileResponse{Resources: n, Clusters: s.clustersOf(ctx, t)}, err
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
