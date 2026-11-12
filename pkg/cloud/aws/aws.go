// Package aws makes the read-only AWS calls the server needs and Crossplane
// cannot (it has no lookup kinds), such as discovering the resources of an
// existing VPC before ImportNetwork creates Observe-only managed resources
// for them. See "Read-only lookups the server makes" in idl/README.md.
//
// Every call runs in a tenant's account. The server starts from its own
// identity in the management account (IRSA in production, the default
// credential chain otherwise), optionally assumes the management role
// first, then assumes the account's tenant role, stored at bootstrap (the
// account's identity).
//
//	ambient credentials ─(SourceRoleARN, optional)─> management role
//	                     ─(tenant role ARN)─> tenant role ─> EC2, ...
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
	"github.com/aws/aws-sdk-go-v2/credentials"
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

// Config says how the server reaches tenant accounts.
type Config struct {
	// SourceRoleARN is assumed first, when set: for running the server
	// with credentials that are not the management role itself (a
	// developer's SSO session). Leave it empty when the server already runs
	// as the management role (IRSA): a role cannot assume itself unless its
	// trust policy says so.
	SourceRoleARN string
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

// New loads the ambient AWS configuration (environment, shared config,
// IRSA web identity, instance role) and returns a Client.
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

// StaticCredentials are an IAM user's access keys.
type StaticCredentials struct {
	AccessKeyID, SecretAccessKey, SessionToken string
}

// Access is how the server reaches an account, mirroring its ProviderConfig:
// start from Static keys (credentials.source Secret) or the server's own
// credentials (IRSA), then assume RoleARN when set.
type Access struct {
	AccountID string
	RoleARN   string
	Static    *StaticCredentials
}

// Tenant is TenantFor(Access{accountID, roleARN}, region): the server's own
// credentials, assuming roleARN unless it is empty.
func (c *Client) Tenant(accountID, roleARN, region string) (*Tenant, error) {
	return c.TenantFor(Access{AccountID: accountID, RoleARN: roleARN}, region)
}

// TenantFor returns a client for one tenant account in one region. RoleARN
// must be a role of that account. It does not call AWS: roles are assumed
// on the first request; VerifyAccess checks where the credentials land.
func (c *Client) TenantFor(a Access, region string) (*Tenant, error) {
	if !accountIDPattern.MatchString(a.AccountID) {
		return nil, fmt.Errorf("%w: account ID must be 12 digits, got %q", ErrInvalid, a.AccountID)
	}
	if a.RoleARN != "" && !strings.HasPrefix(a.RoleARN, "arn:"+awscatalog.PartitionAWS+":iam::"+a.AccountID+":role/") {
		return nil, fmt.Errorf("%w: %q is not a role of account %s", ErrInvalid, a.RoleARN, a.AccountID)
	}
	if !awscatalog.IsRegion(region) {
		return nil, fmt.Errorf("%w: unknown region %q", ErrInvalid, region)
	}
	if a.Static != nil && (a.Static.AccessKeyID == "" || a.Static.SecretAccessKey == "") {
		return nil, fmt.Errorf("%w: static credentials need an access key ID and a secret access key", ErrInvalid)
	}
	cfg := c.base.Copy()
	cfg.Region = region
	switch {
	case a.Static != nil && a.RoleARN != "":
		cfg.Credentials = c.staticAssumed(*a.Static, a.RoleARN, region)
	case a.Static != nil:
		cfg.Credentials = staticProvider(*a.Static)
	case a.RoleARN != "":
		cfg.Credentials = c.tenantCredentials(a.RoleARN, region)
	}
	return &Tenant{AccountID: a.AccountID, Region: region, ec2: ec2.NewFromConfig(cfg), sts: sts.NewFromConfig(cfg),
		eks: eks.NewFromConfig(cfg), logs: cloudwatchlogs.NewFromConfig(cfg), iam: iam.NewFromConfig(cfg)}, nil
}

func staticProvider(s StaticCredentials) awssdk.CredentialsProvider {
	return credentials.NewStaticCredentialsProvider(s.AccessKeyID, s.SecretAccessKey, s.SessionToken)
}

// staticAssumed assumes roleARN with static keys, cached per key, role and
// region.
func (c *Client) staticAssumed(s StaticCredentials, roleARN, region string) *awssdk.CredentialsCache {
	key := "static:" + s.AccessKeyID + "|" + roleARN + "/" + region
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.creds[key]; ok {
		return cached
	}
	source := c.base.Copy()
	source.Region = region
	source.Credentials = staticProvider(s)
	creds := c.assume(source, roleARN)
	c.creds[key] = creds
	return creds
}

func (c *Client) tenantCredentials(roleARN, region string) *awssdk.CredentialsCache {
	key := roleARN + "/" + region
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.creds[key]; ok {
		return cached
	}
	source := c.base.Copy()
	source.Region = region // regional STS endpoint, next to the calls
	if c.cfg.SourceRoleARN != "" {
		source.Credentials = c.assume(source, c.cfg.SourceRoleARN)
	}
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
// VerifyAccess: credentials of another account (e.g. keys in the server's
// environment for a different account) fail with ErrAccessDenied instead
// of reading that account's VPCs.
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
