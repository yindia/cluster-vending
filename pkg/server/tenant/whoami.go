package tenant

import (
	"context"

	"github.com/yindia/cluster-vending/pkg/auth"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// WhoAmI answers with the caller the request's interceptor recognized
// (pkg/server/authn): anonymous where the server does not authenticate.
// There are no roles to report: an accepted caller may call everything.
func (s *TenantService) WhoAmI(ctx context.Context, _ *cloudv1.WhoAmIRequest) (*cloudv1.WhoAmIResponse, error) {
	id := auth.FromContext(ctx)
	return &cloudv1.WhoAmIResponse{Anonymous: id.Anonymous, Subject: id.Subject, Username: id.Username, Issuer: id.Issuer}, nil
}
