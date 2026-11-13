package tenant_test

import (
	"context"
	"slices"
	"testing"

	"github.com/yindia/cluster-vending/pkg/auth"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/tenant"
)

func TestWhoAmI(t *testing.T) {
	svc := tenant.NewTenantService(reconcile.Deps{})
	caller := &auth.Identity{Subject: "sub-1", Username: "object-id", Issuer: "entra", Roles: []string{"Acme.Admin", "Platform.Admin"},
		Grants: []auth.Grant{
			{Role: auth.RolePlatformAdmin},
			{Role: auth.RoleTenantAdmin, Tenants: []string{"acme"}},
			{Role: auth.RoleTenantViewer, Tenants: []string{"*"}},
		}}

	res, err := svc.WhoAmI(auth.WithIdentity(t.Context(), caller), &cloudv1.WhoAmIRequest{})

	if err != nil {
		t.Fatal(err)
	}
	if res.GetAnonymous() || res.GetSubject() != "sub-1" || res.GetUsername() != "object-id" || res.GetIssuer() != "entra" ||
		!slices.Equal(res.GetRoles(), caller.Roles) {
		t.Errorf("WhoAmI = %v", res)
	}
	want := []cloudv1.AccessRole{cloudv1.AccessRole_ACCESS_ROLE_PLATFORM_ADMIN, cloudv1.AccessRole_ACCESS_ROLE_TENANT_ADMIN,
		cloudv1.AccessRole_ACCESS_ROLE_TENANT_VIEWER}
	if len(res.GetGrants()) != len(want) {
		t.Fatalf("grants = %v", res.GetGrants())
	}
	for i, g := range res.GetGrants() {
		if g.GetRole() != want[i] || !slices.Equal(g.GetTenants(), caller.Grants[i].Tenants) {
			t.Errorf("grant %d = %v, want %s for %v", i, g, want[i], caller.Grants[i].Tenants)
		}
	}
}

// Where nothing authenticates, the caller is anonymous.
func TestWhoAmIWithoutAuthentication(t *testing.T) {
	svc := tenant.NewTenantService(reconcile.Deps{})

	res, err := svc.WhoAmI(context.Background(), &cloudv1.WhoAmIRequest{})

	if err != nil || !res.GetAnonymous() || res.GetSubject() != "" || len(res.GetGrants()) != 0 {
		t.Fatalf("WhoAmI = %v, %v; want anonymous", res, err)
	}
}
