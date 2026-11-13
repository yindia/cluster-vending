package tenant

import (
	"context"

	"github.com/yindia/cluster-vending/pkg/auth"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// accessRoles are the contract's names of the roles.
var accessRoles = map[auth.Role]cloudv1.AccessRole{
	auth.RolePlatformAdmin: cloudv1.AccessRole_ACCESS_ROLE_PLATFORM_ADMIN,
	auth.RoleTenantAdmin:   cloudv1.AccessRole_ACCESS_ROLE_TENANT_ADMIN,
	auth.RoleTenantViewer:  cloudv1.AccessRole_ACCESS_ROLE_TENANT_VIEWER,
}

// WhoAmI answers with the caller the request's interceptor recognized
// (pkg/server/authn): anonymous where the server does not authenticate.
func (s *TenantService) WhoAmI(ctx context.Context, _ *cloudv1.WhoAmIRequest) (*cloudv1.WhoAmIResponse, error) {
	id := auth.FromContext(ctx)
	res := &cloudv1.WhoAmIResponse{
		Anonymous: id.Anonymous, Subject: id.Subject, Username: id.Username, Issuer: id.Issuer, Roles: id.Roles,
	}
	for _, g := range id.Grants {
		res.Grants = append(res.Grants, &cloudv1.AccessGrant{Role: accessRoles[g.Role], Tenants: g.Tenants})
	}
	return res, nil
}
