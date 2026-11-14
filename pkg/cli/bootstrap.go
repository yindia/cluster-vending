package cli

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// BootstrapGet calls TenantService.GetCloudAccount for --id, the
// bootstrap ID (CloudAccount.id).
func BootstrapGet(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	if o.ID == "" {
		return nil, errors.New("--id is required")
	}
	return call(ctx, &cloudv1.GetCloudAccountRequest{TenantId: o.Tenant, Id: o.ID}, c.Tenant.GetCloudAccount)
}

// BootstrapVerify calls TenantService.VerifyCloudAccount for --id: the
// controller checks the account's access; get shows the result.
func BootstrapVerify(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	if o.ID == "" {
		return nil, errors.New("--id is required")
	}
	return call(ctx, &cloudv1.VerifyCloudAccountRequest{TenantId: o.Tenant, Id: o.ID, Regions: o.Regions}, c.Tenant.VerifyCloudAccount)
}

// BootstrapList calls TenantService.ListCloudAccounts.
func BootstrapList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.ListCloudAccountsRequest{TenantId: o.Tenant}, c.Tenant.ListCloudAccounts)
}
