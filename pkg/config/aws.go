package config

import (
	"fmt"
	"regexp"
	"strings"

	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
)

// Flag names and environment variables of the AWS settings.
const (
	FlagAWSManagementRoleARN = "aws-management-role-arn"
	EnvAWSManagementRoleARN  = "CV_AWS_MANAGEMENT_ROLE_ARN"
	// FlagProviderCredentialsSecret names the Secret the Crossplane AWS
	// providers authenticate with instead of IRSA (local testing).
	FlagProviderCredentialsSecret = "provider-credentials-secret"
	EnvProviderCredentialsSecret  = "CV_PROVIDER_CREDENTIALS_SECRET" //nolint:gosec // a variable name, not a credential
	FlagSkipTenantRole            = "skip-tenant-role"
	EnvSkipTenantRole             = "CV_SKIP_TENANT_ROLE"
)

// DefaultCredentialsKey is the key of a credentials Secret when the
// setting names none: an AWS shared credentials file.
const DefaultCredentialsKey = "credentials"

// providerCredentialsSetting is the controller's: the Secret the
// providers authenticate with where there is no IRSA.
var providerCredentialsSetting = Setting{Flag: FlagProviderCredentialsSecret, Env: EnvProviderCredentialsSecret,
	Usage: "<namespace>/<name>[:<key>] of a Secret holding AWS keys as a shared credentials file (key default \"credentials\"): every account's ProviderConfig then authenticates the Crossplane AWS providers with it instead of IRSA, and still assumes the tenant role. For a management cluster without IRSA (local kind); empty in production. The controller's own credentials are never a setting: they come from the AWS SDK's default chain (IRSA in production; for local testing export AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, never in a file or flag)"}

// SecretKeyRef names one key of a Secret.
type SecretKeyRef struct {
	Namespace, Name, Key string
	// Direct: the keys are used as they are, without assuming the tenant
	// role (FlagSkipTenantRole; local testing only).
	Direct bool
}

// String is the setting's own spelling of the reference.
func (r SecretKeyRef) String() string { return r.Namespace + "/" + r.Name + ":" + r.Key }

var (
	// A namespace is a DNS label; a Secret name a DNS subdomain.
	dnsLabelPattern     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dnsSubdomainPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	secretKeyPattern    = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)
)

// parseSecretKeyRef reads <namespace>/<name>[:<key>]; nil for "".
func parseSecretKeyRef(raw string) (*SecretKeyRef, error) {
	if raw == "" {
		return nil, nil
	}
	ref, key, hasKey := strings.Cut(raw, ":")
	ns, name, ok := strings.Cut(ref, "/")
	if !hasKey {
		key = DefaultCredentialsKey
	}
	switch {
	case !ok || !dnsLabelPattern.MatchString(ns) || !dnsSubdomainPattern.MatchString(name):
		return nil, fmt.Errorf("%s must be <namespace>/<name>[:<key>], got %q", setting(FlagProviderCredentialsSecret), raw)
	case !secretKeyPattern.MatchString(key):
		return nil, fmt.Errorf("%s: %q is not a Secret key", setting(FlagProviderCredentialsSecret), key)
	}
	return &SecretKeyRef{Namespace: ns, Name: name, Key: key}, nil
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
	// ProviderCredentials, when set, is the Secret the Crossplane AWS
	// providers authenticate with instead of IRSA
	// (FlagProviderCredentialsSecret); nil in production.
	ProviderCredentials *SecretKeyRef
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
	}
	return errs
}
