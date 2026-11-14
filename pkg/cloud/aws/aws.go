// Package aws makes the read-only AWS calls the controller needs and
// Crossplane cannot (it has no lookup kinds), such as discovering the
// resources of an existing VPC so that an import can create Observe-only
// managed resources for them (the calls are listed at AwsImportNetwork in
// idl/cloud/v1/aws.proto). The API server makes none of them.
//
// Every call runs in a tenant's account, and there is one way in. The
// controller starts from its own credentials, as the AWS SDK's default
// chain finds them (IRSA in production: the web identity of its service
// account; keys in its environment for local testing), and then ALWAYS
// assumes the account's tenant role, stored at bootstrap (the account's
// identity). Nothing is read with the
// base credentials themselves: keys of one account cannot read another,
// and a tenant grants access in one place, its role's trust policy.
//
//	own credentials ─(tenant role ARN, always)─> tenant role ─> EC2, ...
package aws

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
)

// DefaultSessionName is the role session name of every assumed role; it
// shows up in the tenant's CloudTrail as the platform's calls.
const DefaultSessionName = "cluster-vending"

// sessionDuration is how long assumed-role credentials last. The cache
// refreshes them shortly before they expire.
const sessionDuration = time.Hour

var (
	accountIDPattern = regexp.MustCompile(`^\d{12}$`)
	// IAM role session names: 2-64 of letters, digits and +=,.@_-.
	sessionNamePattern = regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)
)

// Errors returned by every call, to be mapped to Connect codes.
var (
	// ErrNotFound: a resource the caller named does not exist in the
	// tenant's account and region.
	ErrNotFound = errors.New("aws: not found")
	// ErrAccessDenied: the tenant role cannot be assumed, or does not allow
	// the call. Typically the tenant has not (or no longer) granted access.
	ErrAccessDenied = errors.New("aws: access denied")
	// ErrInvalid: bad input (account ID, region, resource ID).
	ErrInvalid = errors.New("aws: invalid")
)

// Config says how the controller reaches tenant accounts.
type Config struct {
	// SessionName defaults to DefaultSessionName.
	SessionName string
}

func (c Config) withDefaults() (Config, error) {
	if c.SessionName == "" {
		c.SessionName = DefaultSessionName
	}
	if !sessionNamePattern.MatchString(c.SessionName) {
		return c, fmt.Errorf("%w: role session name %q", ErrInvalid, c.SessionName)
	}
	return c, nil
}

// Client hands out per-tenant clients. It caches assumed-role credentials
// per role and region, so tenants share nothing and repeated calls do
// not call STS again. Safe for concurrent use.
type Client struct {
	base awssdk.Config
	cfg  Config

	mu    sync.Mutex
	creds map[string]*awssdk.CredentialsCache // key: roleARN/region
}

// New loads the process's own AWS configuration with the SDK's default
// chain (keys in the environment, shared config, the IRSA web identity,
// an instance role) and returns a Client that starts from it.
func New(ctx context.Context, cfg Config, optFns ...func(*awsconfig.LoadOptions) error) (*Client, error) {
	base, err := awsconfig.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("aws: load configuration: %w", err)
	}
	return NewFromConfig(base, cfg)
}

// NewFromConfig returns a Client that starts from base's credentials.
func NewFromConfig(base awssdk.Config, cfg Config) (*Client, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Client{base: base, cfg: cfg, creds: map[string]*awssdk.CredentialsCache{}}, nil
}

// Access is where a lookup goes: one tenant account, through its tenant
// role. There is no way to a tenant's account around its role.
type Access struct {
	AccountID string
	// RoleARN is the tenant role, a role of AccountID. Required unless
	// Direct.
	RoleARN string
	// Direct: the client's own credentials are used as they are, without
	// assuming a role (local testing, --skip-tenant-role). RoleARN must
	// then be empty.
	Direct bool
}

// Tenant is TenantFor(Access{accountID, roleARN}, region).
func (c *Client) Tenant(accountID, roleARN, region string) (*Tenant, error) {
	return c.TenantFor(Access{AccountID: accountID, RoleARN: roleARN}, region)
}

// TenantFor returns a client for one tenant account in one region, whose
// calls are made as the account's tenant role: the role is assumed with
// the client's own credentials (through the source role, when one is
// configured). RoleARN must be a role of that account. It does not call
// AWS: roles are assumed on the first request; VerifyAccess checks where
// the credentials land.
func (c *Client) TenantFor(a Access, region string) (*Tenant, error) {
	if !accountIDPattern.MatchString(a.AccountID) {
		return nil, fmt.Errorf("%w: account ID must be 12 digits, got %q", ErrInvalid, a.AccountID)
	}
	switch {
	case a.Direct && a.RoleARN != "":
		return nil, fmt.Errorf("%w: account %s: a direct access assumes no role, got %q", ErrInvalid, a.AccountID, a.RoleARN)
	case !a.Direct && a.RoleARN == "":
		return nil, fmt.Errorf("%w: account %s has no tenant role: a tenant's account is only reached through its role", ErrInvalid, a.AccountID)
	}
	if !a.Direct && !strings.HasPrefix(a.RoleARN, "arn:"+awscatalog.PartitionAWS+":iam::"+a.AccountID+":role/") {
		return nil, fmt.Errorf("%w: %q is not a role of account %s", ErrInvalid, a.RoleARN, a.AccountID)
	}
	cfg, err := c.regionConfig(a, region)
	if err != nil {
		return nil, err
	}
	eksClient, iamClient := eks.NewFromConfig(cfg), iam.NewFromConfig(cfg)
	return &Tenant{AccountID: a.AccountID, Region: region, ec2: ec2.NewFromConfig(cfg), sts: sts.NewFromConfig(cfg),
		eks: eksClient, eksImport: eksClient, logs: cloudwatchlogs.NewFromConfig(cfg), iam: iamClient, iamImport: iamClient}, nil
}

// regionConfig is the SDK configuration of a in region: the client's own
// credentials, through the tenant role unless the access is direct.
func (c *Client) regionConfig(a Access, region string) (awssdk.Config, error) {
	if !awscatalog.IsRegion(region) {
		return awssdk.Config{}, fmt.Errorf("%w: unknown region %q", ErrInvalid, region)
	}
	cfg := c.base.Copy()
	cfg.Region = region
	if !a.Direct {
		cfg.Credentials = c.tenantCredentials(a.RoleARN, region)
	}
	return cfg, nil
}

// tenantCredentials are the credentials of a tenant role, cached per role
// and region, assumed with the client's own credentials.
func (c *Client) tenantCredentials(roleARN, region string) *awssdk.CredentialsCache {
	key := roleARN + "/" + region
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.creds[key]; ok {
		return cached
	}
	source := c.base.Copy()
	source.Region = region // regional STS endpoint, next to the calls
	creds := c.assume(source, roleARN)
	c.creds[key] = creds
	return creds
}

// assume returns cached credentials of roleARN, assumed with from's
// credentials.
func (c *Client) assume(from awssdk.Config, roleARN string) *awssdk.CredentialsCache {
	provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(from), roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = c.cfg.SessionName
		o.Duration = sessionDuration
	})
	return awssdk.NewCredentialsCache(provider)
}

// Tenant makes calls in one tenant account and region.
type Tenant struct {
	AccountID string
	Region    string
	ec2       EC2API
	sts       STSAPI
	eks       EKSAPI
	logs      LogsAPI
	iam       IAMAPI
	eksImport EKSImportAPI
	iamImport IAMImportAPI
}

// STSAPI is the part of the STS client VerifyAccess uses.
type STSAPI interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// VerifyAccess assumes the tenant role and checks that it lands in the
// tenant's account (sts:GetCallerIdentity needs no permission). It fails
// with ErrAccessDenied when the role does not exist or does not trust the
// platform.
func (t *Tenant) VerifyAccess(ctx context.Context) error {
	out, err := t.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return translate(err, "verify access to account "+t.AccountID)
	}
	if got := awssdk.ToString(out.Account); got != t.AccountID {
		return fmt.Errorf("%w: the credentials are for account %s (%s), but the bootstrapped account is %s",
			ErrAccessDenied, got, awssdk.ToString(out.Arn), t.AccountID)
	}
	return nil
}

// asError is errors.As.
func asError(err error, target any) bool { return errors.As(err, target) }

// translate wraps AWS errors in the package errors; others pass through.
func translate(err error, what string) error {
	if err == nil {
		return nil
	}
	// AssumeRole failures (tenant role missing, trust policy) are
	// AccessDenied API errors too, from STS.
	var api smithy.APIError
	if errors.As(err, &api) {
		switch code := api.ErrorCode(); {
		case code == "AccessDenied", code == "AccessDeniedException",
			code == "UnauthorizedOperation", code == "AuthFailure":
			return fmt.Errorf("%w: %s: %w", ErrAccessDenied, what, err)
		case notFoundCode.MatchString(code):
			return fmt.Errorf("%w: %s: %w", ErrNotFound, what, err)
		case malformedCode.MatchString(code):
			return fmt.Errorf("%w: %s: %w", ErrInvalid, what, err)
		}
	}
	return fmt.Errorf("aws: %s: %w", what, err)
}

var (
	// e.g. InvalidVpcID.NotFound, InvalidSubnetID.NotFound.
	notFoundCode = regexp.MustCompile(`\.NotFound$`)
	// e.g. InvalidVpcID.Malformed.
	malformedCode = regexp.MustCompile(`\.Malformed$`)
)

// NetworkDiscoverer looks an existing network up (ImportNetwork).
type NetworkDiscoverer interface {
	DiscoverNetwork(ctx context.Context, access Access, region, vpcID string, subnetIDs []string) (*Network, error)
}

var _ NetworkDiscoverer = (*Client)(nil)

// DiscoverNetwork is TenantFor(access, region).DiscoverNetwork, after
// VerifyAccess: credentials that do not land in the tenant's account fail
// with ErrAccessDenied instead of reading another account's VPCs.
func (c *Client) DiscoverNetwork(ctx context.Context, access Access, region, vpcID string, subnetIDs []string) (*Network, error) {
	t, err := c.TenantFor(access, region)
	if err != nil {
		return nil, err
	}
	if err := t.VerifyAccess(ctx); err != nil {
		return nil, err
	}
	return t.DiscoverNetwork(ctx, vpcID, subnetIDs)
}
