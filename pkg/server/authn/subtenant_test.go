package authn_test

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/auth"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

type subTenantHandlers struct {
	cloudv1connect.UnimplementedSubTenantServiceHandler
	observed *handlers
}

func (h subTenantHandlers) GetSubTenant(ctx context.Context, _ *cloudv1.GetSubTenantRequest) (*cloudv1.GetSubTenantResponse, error) {
	h.observed.seen(ctx)
	return &cloudv1.GetSubTenantResponse{}, nil
}

func (h subTenantHandlers) CreateSubTenant(ctx context.Context, _ *cloudv1.CreateSubTenantRequest) (*cloudv1.CreateSubTenantResponse, error) {
	h.observed.seen(ctx)
	return &cloudv1.CreateSubTenantResponse{}, nil
}

func TestSubTenantAuthorization(t *testing.T) {
	a := newAPI(t, auth.ModeRequired)
	for _, tt := range []struct {
		name, role, tenant string
		write              bool
		want               connect.Code
	}{
		{"viewer reads own tenant", "acme.viewer", "acme", false, 0},
		{"viewer cannot create", "acme.viewer", "acme", true, connect.CodePermissionDenied},
		{"admin creates own tenant", "acme.admin", "acme", true, 0},
		{"admin cannot read another tenant", "acme.admin", "globex", false, connect.CodePermissionDenied},
		{"admin cannot write another tenant", "acme.admin", "globex", true, connect.CodePermissionDenied},
		{"missing tenant refused", "acme.admin", "", false, connect.CodePermissionDenied},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := with(t, bearer(a.token(t, tt.role)))
			before := a.handlers.reached.Load()
			var err error
			if tt.write {
				_, err = a.clients.subtenant.CreateSubTenant(ctx, &cloudv1.CreateSubTenantRequest{TenantId: tt.tenant})
			} else {
				_, err = a.clients.subtenant.GetSubTenant(ctx, &cloudv1.GetSubTenantRequest{TenantId: tt.tenant})
			}
			if (err == nil) != (tt.want == 0) || (err != nil && connect.CodeOf(err) != tt.want) {
				t.Fatalf("call = %v, want %v", err, tt.want)
			}
			if reached := a.handlers.reached.Load() - before; (reached == 1) != (tt.want == 0) {
				t.Fatalf("handler reached %d times, want allowed=%v", reached, tt.want == 0)
			}
		})
	}
}
