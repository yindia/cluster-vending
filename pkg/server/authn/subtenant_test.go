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

// Any authenticated caller reaches the sub-tenant RPCs of any tenant.
func TestSubTenantCallsNeedOnlyAToken(t *testing.T) {
	a := newAPI(t, auth.ModeRequired)
	for _, tt := range []struct {
		name, tenant string
		write        bool
	}{
		{"read", "acme", false},
		{"create", "acme", true},
		{"another tenant's read", "globex", false},
		{"another tenant's create", "globex", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := with(t, bearer(a.token(t)))
			before := a.handlers.reached.Load()
			var err error
			if tt.write {
				_, err = a.clients.subtenant.CreateSubTenant(ctx, &cloudv1.CreateSubTenantRequest{TenantId: tt.tenant})
			} else {
				_, err = a.clients.subtenant.GetSubTenant(ctx, &cloudv1.GetSubTenantRequest{TenantId: tt.tenant})
			}
			if err != nil {
				t.Fatalf("call = %v, want it to reach the handler", err)
			}
			if reached := a.handlers.reached.Load() - before; reached != 1 {
				t.Fatalf("handler reached %d times", reached)
			}
		})
	}
	if _, err := a.clients.subtenant.GetSubTenant(with(t, ""), &cloudv1.GetSubTenantRequest{TenantId: "acme"}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("no token = %v, want UNAUTHENTICATED", err)
	}
}
