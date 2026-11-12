package reconcile

import (
	"context"
	"strings"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
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
// the account.
func (d Deps) AWSAccess(_ context.Context, a *model.CloudAccount) (awscloud.Access, error) {
	if a.Identity == "" {
		return awscloud.Access{}, fault.Newf(fault.FailedPrecondition, "account %s has no tenant role yet; bootstrap it again", a.ID)
	}
	if !strings.HasPrefix(a.Identity, "arn:"+awscatalog.PartitionAWS+":iam::"+a.AccountID+":role/") {
		return awscloud.Access{}, fault.Newf(fault.FailedPrecondition,
			"account %s: its tenant role %s is not a role of AWS account %s; bootstrap it again", a.ID, a.Identity, a.AccountID)
	}
	return awscloud.Access{AccountID: a.AccountID, RoleARN: a.Identity}, nil
}
