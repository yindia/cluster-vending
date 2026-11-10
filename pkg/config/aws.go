package config

import (
	"fmt"
	"regexp"

	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
)

// Flag names and environment variables of the AWS settings.
const (
	FlagAWSManagementAccountID = "aws-management-account-id"
	EnvAWSManagementAccountID  = "CV_AWS_MANAGEMENT_ACCOUNT_ID"
	FlagAWSManagementRoleARN   = "aws-management-role-arn"
	EnvAWSManagementRoleARN    = "CV_AWS_MANAGEMENT_ROLE_ARN"
)

var awsSettings = []Setting{
	{Flag: FlagAWSManagementAccountID, Env: EnvAWSManagementAccountID,
		Usage: "AWS account ID (12 digits) of the management cluster; required"},
	{Flag: FlagAWSManagementRoleARN, Env: EnvAWSManagementRoleARN,
		Usage: "IAM role ARN the Crossplane AWS provider runs as (IRSA), in the management account; tenant roles trust it; required"},
}

var (
	accountIDPattern = regexp.MustCompile(`^\d{12}$`)
	// arn:<partition>:iam::<account>:role/[<path>/]<name>
	roleARNPattern = regexp.MustCompile(`^arn:([a-z-]+):iam::(\d{12}):role/(?:[\x21-\x7e]*/)?[\w+=,.@-]{1,64}$`)
	// IAM role names: letters, digits and +=,.@_- (\w covers the underscore).
	roleNamePattern = regexp.MustCompile(`^[\w+=,.@-]{1,64}$`)
)

// AWS is how the platform reaches tenant AWS accounts. The Crossplane AWS
// provider runs as ManagementRoleARN (IRSA) in the management account, and
// each account's ProviderConfig chains from it into the role named at
// bootstrap (AwsAccount.role_name; see idl/README.md, "Account access").
type AWS struct {
	ManagementAccountID string
	ManagementRoleARN   string
	// StaticKeys are AWS keys from the environment (AWS_ACCESS_KEY_ID,
	// AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN). When set, the server's own
	// AWS calls use them directly for every account (local testing); else
	// they follow each account's ProviderConfig. Never logged.
	StaticKeys *StaticKeys
}

// StaticKeys are AWS access keys.
type StaticKeys struct {
	AccessKeyID, SecretAccessKey, SessionToken string
}

// Environment variables of the static keys (the AWS SDK's names).
const (
	EnvAWSAccessKeyID     = "AWS_ACCESS_KEY_ID"
	EnvAWSSecretAccessKey = "AWS_SECRET_ACCESS_KEY" //nolint:gosec // a variable name, not a credential
	EnvAWSSessionToken    = "AWS_SESSION_TOKEN"     //nolint:gosec // a variable name, not a credential
)

// staticKeysFrom reads the keys from env; nil unless both the key ID and
// the secret are set.
func staticKeysFrom(env LookupFunc) *StaticKeys {
	id, _ := get(env, EnvAWSAccessKeyID)
	secret, _ := get(env, EnvAWSSecretAccessKey)
	if id == "" || secret == "" {
		return nil
	}
	token, _ := get(env, EnvAWSSessionToken)
	return &StaticKeys{AccessKeyID: id, SecretAccessKey: secret, SessionToken: token}
}

// TenantRoleARN returns the ARN of roleName in a tenant's AWS account.
func TenantRoleARN(accountID, roleName string) (string, error) {
	if !accountIDPattern.MatchString(accountID) {
		return "", fmt.Errorf("tenant account ID must be 12 digits, got %q", accountID)
	}
	if !roleNamePattern.MatchString(roleName) {
		return "", fmt.Errorf("IAM role name must be 1-64 of letters, digits, +=,.@_-, got %q", roleName)
	}
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", awscatalog.PartitionAWS, accountID, roleName), nil
}

func (a AWS) validate() []error {
	var errs []error
	accountOK := false
	switch {
	case a.ManagementAccountID == "":
		errs = append(errs, fmt.Errorf("%s is required", setting(FlagAWSManagementAccountID)))
	case !accountIDPattern.MatchString(a.ManagementAccountID):
		errs = append(errs, fmt.Errorf("%s must be 12 digits, got %q", setting(FlagAWSManagementAccountID), a.ManagementAccountID))
	default:
		accountOK = true
	}

	if a.ManagementRoleARN == "" {
		errs = append(errs, fmt.Errorf("%s is required", setting(FlagAWSManagementRoleARN)))
	} else if m := roleARNPattern.FindStringSubmatch(a.ManagementRoleARN); m == nil {
		errs = append(errs, fmt.Errorf("%s must be an IAM role ARN (arn:aws:iam::<account>:role/<name>), got %q",
			setting(FlagAWSManagementRoleARN), a.ManagementRoleARN))
	} else {
		if partition := m[1]; partition != awscatalog.PartitionAWS {
			errs = append(errs, fmt.Errorf("%s must be in the %q partition, got %q",
				setting(FlagAWSManagementRoleARN), awscatalog.PartitionAWS, partition))
		}
		if account := m[2]; accountOK && account != a.ManagementAccountID {
			errs = append(errs, fmt.Errorf("%s is in account %s, want the management account %s",
				setting(FlagAWSManagementRoleARN), account, a.ManagementAccountID))
		}
	}
	return errs
}
