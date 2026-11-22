package tenant_test

import (
	"context"
	"testing"

	"github.com/yindia/cluster-vending/pkg/auth"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/tenant"
)

// WhoAmI is the identity only: the API has no roles to report.
func TestWhoAmI(t *testing.T) {
	svc := tenant.NewTenantService(reconcile.Deps{})
	caller := &auth.Identity{Subject: "sub-1", Username: "object-id", Issuer: "entra"}

	res, err := svc.WhoAmI(auth.WithIdentity(t.Context(), caller), &cloudv1.WhoAmIRequest{})

	if err != nil {
		t.Fatal(err)
	}
	if res.GetAnonymous() || res.GetSubject() != "sub-1" || res.GetUsername() != "object-id" || res.GetIssuer() != "entra" {
		t.Errorf("WhoAmI = %v", res)
	}
}

// Where nothing authenticates, the caller is anonymous.
func TestWhoAmIWithoutAuthentication(t *testing.T) {
	svc := tenant.NewTenantService(reconcile.Deps{})

	res, err := svc.WhoAmI(context.Background(), &cloudv1.WhoAmIRequest{})

	if err != nil || !res.GetAnonymous() || res.GetSubject() != "" {
		t.Fatalf("WhoAmI = %v, %v; want anonymous", res, err)
	}
}
