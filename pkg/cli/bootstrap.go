package cli

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// BootstrapCreate calls TenantService.BootstrapTenant with the
// CloudAccount manifest (metadata.name: the account name; spec: aws:
// {accountId, role}).
func BootstrapCreate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadManifest(o.Input, KindCloudAccount)
	if err != nil {
		return nil, err
	}
	tenant, err := m.tenant(o.Tenant)
	if err != nil {
		return nil, err
	}
	account, err := m.CloudAccount()
	if err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.BootstrapTenantRequest{TenantId: tenant, Account: account}, c.Tenant.BootstrapTenant)
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
