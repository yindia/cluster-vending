package cli

import (
	"context"
	"errors"
	"fmt"

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
// controller checks the account's access; get shows the result. With
// --wait it waits for the controller's verdict and prints the account: a
// denied check exits non-zero.
func BootstrapVerify(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	if o.ID == "" {
		return nil, errors.New("--id is required")
	}
	if !o.Wait {
		return call(ctx, &cloudv1.VerifyCloudAccountRequest{TenantId: o.Tenant, Id: o.ID, Regions: o.Regions}, c.Tenant.VerifyCloudAccount)
	}
	res, err := call(ctx, &cloudv1.GetCloudAccountRequest{TenantId: o.Tenant, Id: o.ID}, c.Tenant.GetCloudAccount)
	if err != nil {
		return nil, err
	}
	checked, err := checkAccess(ctx, c, o, res.(*cloudv1.GetCloudAccountResponse).GetAccount())
	if err != nil {
		return nil, err
	}
	if err := Print(o.Out, &cloudv1.GetCloudAccountResponse{Account: checked}, o.Output); err != nil {
		return nil, err
	}
	access := checked.GetStatus().GetAccess()
	if !access.GetVerified() {
		return nil, fmt.Errorf("the access check failed: %s: %s; fix the role %s in account %s, then run account verify again",
			access.GetReason(), access.GetMessage(), checked.GetAws().GetRole(), checked.GetAws().GetAccountId())
	}
	if o.Output == OutputTable || o.Output == OutputWide {
		_, err = fmt.Fprintf(o.Out, "\nNext: cluster-vending network create -f <Network manifest with spec.bootstrapId: %s> --tenant %s\n",
			checked.GetId(), checked.GetTenantId())
	}
	return nil, err
}

// checkAccess asks the controller to check the account's access (and
// --region's regions) and waits for its verdict: a check later than the
// one the account has now.
func checkAccess(ctx context.Context, c Clients, o Options, a *cloudv1.CloudAccount) (*cloudv1.CloudAccount, error) {
	before := a.GetStatus().GetAccess().GetCheckedTime().AsTime()
	if _, err := call(ctx, &cloudv1.VerifyCloudAccountRequest{TenantId: a.GetTenantId(), Id: a.GetId(), Regions: o.Regions},
		c.Tenant.VerifyCloudAccount); err != nil {
		return nil, err
	}
	o.progressf("Checking the account's access: the controller assumes %s in account %s\n",
		a.GetAws().GetRole(), a.GetAws().GetAccountId())
	latest := a
	err := poll(ctx, o, "the access check", func() (bool, error) {
		res, err := call(ctx, &cloudv1.GetCloudAccountRequest{TenantId: a.GetTenantId(), Id: a.GetId()}, c.Tenant.GetCloudAccount)
		if err != nil {
			return false, err
		}
		latest = res.(*cloudv1.GetCloudAccountResponse).GetAccount()
		access := latest.GetStatus().GetAccess()
		return access != nil && access.GetCheckedTime().AsTime().After(before), nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: account get --id %s shows the check when it is done", err, a.GetId())
	}
	if access := latest.GetStatus().GetAccess(); access.GetVerified() {
		o.progressf("Access: verified, as %s\n", access.GetPrincipalArn())
	}
	return latest, nil
}

// BootstrapList calls TenantService.ListCloudAccounts.
func BootstrapList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.ListCloudAccountsRequest{TenantId: o.Tenant}, c.Tenant.ListCloudAccounts)
}
