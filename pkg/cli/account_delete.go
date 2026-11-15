package cli

import (
	"context"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// AccountDelete deletes the account --id (TenantService.DeleteCloudAccount);
// --wait waits until it is gone. The role in AWS stays: it is the
// tenant's to delete (aws cloudformation delete-stack).
func AccountDelete(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	if err := RequireID(o.ID, "account"); err != nil {
		return nil, err
	}
	deleted, err := call(ctx, &cloudv1.DeleteCloudAccountRequest{TenantId: o.Tenant, Id: o.ID}, c.Tenant.DeleteCloudAccount)
	if err != nil {
		return nil, err
	}
	account := deleted.(*cloudv1.DeleteCloudAccountResponse).GetAccount()
	o.progressf("Deleting the account %s (%s): the controller removes its ProviderConfig; the role %s stays in account %s\n",
		account.GetAccountName(), o.ID, account.GetAws().GetRole(), account.GetAws().GetAccountId())
	if !o.Wait {
		return deleted, nil
	}
	err = poll(ctx, o, "the account "+o.ID, func() (bool, error) {
		_, err := call(ctx, &cloudv1.GetCloudAccountRequest{TenantId: o.Tenant, Id: o.ID}, c.Tenant.GetCloudAccount)
		if connect.CodeOf(err) == connect.CodeNotFound {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		return nil, err
	}
	o.progressf("Account %s: deleted\n", o.ID)
	return deleted, nil
}
