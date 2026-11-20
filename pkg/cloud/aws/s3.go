package aws

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
)

// AccessFor is the Access to an account from what its bootstrap stored: the
// AWS account ID, the tenant role's ARN (the account's identity) and its
// external ID, empty for none. It only says where to go and makes no call.
// The role must be a role of the account (ErrInvalid otherwise, also when
// the account has none yet): a tenant's account is only reached through its
// own role. The controller (reconcile.Deps.AWSAccess) and the API server
// both make an account's Access with it.
func AccessFor(accountID, tenantRoleARN, externalID string) (Access, error) {
	if !accountIDPattern.MatchString(accountID) {
		return Access{}, fmt.Errorf("%w: account ID must be 12 digits, got %q", ErrInvalid, accountID)
	}
	if tenantRoleARN == "" {
		return Access{}, fmt.Errorf("%w: account %s has no tenant role yet; bootstrap it again", ErrInvalid, accountID)
	}
	if !strings.HasPrefix(tenantRoleARN, "arn:"+awscatalog.PartitionAWS+":iam::"+accountID+":role/") {
		return Access{}, fmt.Errorf("%w: account %s: its tenant role %s is not a role of AWS account %s; bootstrap it again",
			ErrInvalid, accountID, tenantRoleARN, accountID)
	}
	return Access{AccountID: accountID, RoleARN: tenantRoleARN, ExternalID: externalID}, nil
}

// S3 returns an S3 client for one tenant account in one region, whose calls
// are made as the account's tenant role (the client's own credentials, then
// the source role when one is configured, then the tenant role, as for
// TenantFor). The client is what pkg/results reads and writes the results
// buckets with. It does not call AWS: the role is assumed on the first
// request, and a denial then is an error of that request (results.
// ErrAccessDenied).
func (c *Client) S3(a Access, region string) (*s3.Client, error) {
	if err := c.checkAccess(a); err != nil {
		return nil, err
	}
	cfg, err := c.regionConfig(a, region)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(cfg), nil
}
