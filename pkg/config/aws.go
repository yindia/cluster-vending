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
	FlagAWSSourceRoleARN       = "aws-source-role-arn"
	EnvAWSSourceRoleARN        = "CV_AWS_SOURCE_ROLE_ARN"
)

var awsSettings = []Setting{
	{Flag: FlagAWSManagementAccountID, Env: EnvAWSManagementAccountID,
		Usage: "AWS account ID (12 digits) of the management cluster; required by what reconciles (the controller, or the server with the background loop)"},
	{Flag: FlagAWSManagementRoleARN, Env: EnvAWSManagementRoleARN,
		Usage: "IAM role ARN the Crossplane AWS provider runs as (IRSA), in the management account; tenant roles trust it; required by what reconciles"},
}

// awsSourceSetting is the role between the controller's own credentials
// and the tenant roles.
var awsSourceSetting = Setting{Flag: FlagAWSSourceRoleARN, Env: EnvAWSSourceRoleARN,
	Usage: "IAM role ARN assumed with the controller's own credentials before every tenant role; empty when those credentials already are the role the tenant roles trust (IRSA as the management role). The credentials themselves are never a setting: they come from the AWS SDK's default chain (IRSA in production; for local testing export AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY in the environment, never in a file or flag)"}

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
	// SourceRoleARN, when set, is assumed with the controller's own
	// credentials before every tenant role (FlagAWSSourceRoleARN).
	SourceRoleARN string
	// StaticKeys are AWS keys found in the environment (AWS_ACCESS_KEY_ID,
	// AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN): local testing. They are
	// only noticed here, to say so in the log; the AWS SDK reads them
	// itself as the controller's own credentials, and every lookup still
	// assumes the tenant role of the account it reads. Never logged.
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
	if a.SourceRoleARN != "" && !roleARNPattern.MatchString(a.SourceRoleARN) {
		errs = append(errs, fmt.Errorf("%s must be an IAM role ARN (arn:aws:iam::<account>:role/<name>), got %q",
			setting(FlagAWSSourceRoleARN), a.SourceRoleARN))
	}
	return errs
}
