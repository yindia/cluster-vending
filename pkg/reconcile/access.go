package reconcile

import (
	"context"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
)

// AWSAccess is how the reconcilers' own AWS lookups (the discovery of an
// import, the EKS name and version checks) reach a tenant's account. There
// is one rule:
//
//	the controller's own credentials ─(source role, optional)─> tenant role
//
// The controller's own credentials are whatever the AWS SDK's default
// chain finds: keys in its environment for local testing, IRSA (the web
// identity of its service account) in production. Whatever they are, the
// tenant role of the account is always assumed: the role stored at
// bootstrap (the account's identity, the one its ProviderConfig assumes
// too). Nothing is ever read with the base credentials themselves, so
// keys of one account cannot read another, and a tenant grants access in
// one place: its role's trust policy.
//
// It only says where to go; it makes no call. The role must be a role of
// the account (awscloud.AccessFor, which the API server uses too).
func (d Deps) AWSAccess(_ context.Context, a *model.CloudAccount) (awscloud.Access, error) {
	// The one rule for the controller and the API server alike.
	access, err := awscloud.AccessFor(a.AccountID, a.Identity, a.ExternalID)
	if err != nil {
		return awscloud.Access{}, fault.New(fault.FailedPrecondition, err.Error())
	}
	if d.ProviderCredentials != nil && d.ProviderCredentials.Direct {
		// Local testing (--skip-tenant-role): the controller's own keys,
		// as they are.
		return awscloud.Access{AccountID: a.AccountID, Direct: true}, nil
	}
	return access, nil
}
