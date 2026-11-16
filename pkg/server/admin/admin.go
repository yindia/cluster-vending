// Package admin implements the cloud.v1 AdminService: operations of the
// platform's operators on tenants' managed resources.
package admin

import (
	"context"
	"log/slog"
	"time"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/reconcile"
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

func (s *Service) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}
