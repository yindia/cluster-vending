package cli

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// BootstrapCreate calls TenantService.BootstrapTenant with the CloudAccount
// in the input file (accountName, aws: {accountId}).
func BootstrapCreate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	if o.Input == "" {
		return nil, errors.New("--config is required: the account to bootstrap")
	}
	account := &cloudv1.CloudAccount{}
	if err := LoadInput(o.Input, account); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.BootstrapTenantRequest{TenantId: o.Tenant, Account: account}, c.Tenant.BootstrapTenant)
}

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

// BootstrapList calls TenantService.ListCloudAccounts.
func BootstrapList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.ListCloudAccountsRequest{TenantId: o.Tenant}, c.Tenant.ListCloudAccounts)
}
