package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// callerTimeout bounds the AWS calls that find who runs cluster create:
// without credentials the SDK's lookup (e.g. the instance metadata
// endpoint) must not hold the command up.
const callerTimeout = 15 * time.Second

// CallerPrincipal finds the IAM principal of the caller's AWS credentials
// (sts:GetCallerIdentity) as EKS access entries take it: a user ARN as it
// is, an assumed-role session as its role's ARN. note says what could not
// be known (a role's path). region is used when the credentials name none.
type CallerPrincipal func(ctx context.Context, region string) (principal, note string, err error)

// errNoCaller is a caller whose credentials are missing or cannot be used.
var errNoCaller = errors.New("no usable AWS credentials")

// principalFromCaller is the IAM principal of a GetCallerIdentity ARN:
//
//	arn:aws:iam::ACCT:user/PATH/NAME                 as it is
//	arn:aws:sts::ACCT:assumed-role/ROLE/SESSION      arn:aws:iam::ACCT:role/ROLE
//
// role is the role's name for an assumed role (its path is not in the
// session ARN), empty otherwise. The account root and federated users
// cannot be EKS access entries.
func principalFromCaller(arn string) (principal, role string, err error) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[4] == "" {
		return "", "", fmt.Errorf("%q is not an IAM ARN", arn)
	}
	partition, service, account, resource := parts[1], parts[2], parts[4], parts[5]
	switch {
	case service == "iam" && strings.HasPrefix(resource, "user/"):
		return arn, "", nil
	case service == "iam" && strings.HasPrefix(resource, "role/"):
		return arn, "", nil
	case service == "sts" && strings.HasPrefix(resource, "assumed-role/"):
		name, _, _ := strings.Cut(strings.TrimPrefix(resource, "assumed-role/"), "/")
		if name == "" {
			return "", "", fmt.Errorf("%q names no role", arn)
		}
		return "arn:" + partition + ":iam::" + account + ":role/" + name, name, nil
	}
	return "", "", fmt.Errorf("%s cannot be a cluster admin (EKS access entries take IAM users and roles only)", arn)
}

// awsCallerPrincipal is CallerPrincipal with the default AWS credential
// chain. A role's path comes from iam:GetRole; without that permission
// the role's ARN has no path, and note says so.
func awsCallerPrincipal(ctx context.Context, region string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, callerTimeout)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errNoCaller, err)
	}
	if cfg.Region == "" {
		cfg.Region = region
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return "", "", fmt.Errorf("%w: %w", errNoCaller, err)
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", "", fmt.Errorf("%w: sts:GetCallerIdentity: %w", errNoCaller, err)
	}
	principal, role, err := principalFromCaller(aws.ToString(out.Arn))
	if err != nil || role == "" {
		return principal, "", err
	}
	got, err := iam.NewFromConfig(cfg).GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
	if err != nil || got.Role == nil || aws.ToString(got.Role.Arn) == "" {
		return principal, fmt.Sprintf("the path of role %s is unknown (iam:GetRole was refused); if the role has one, "+
			"put its full ARN in spec.access.adminPrincipals instead", role), nil
	}
	return aws.ToString(got.Role.Arn), "", nil
}

// addSelfAdmin makes the caller of cluster create an admin of the new
// cluster: its IAM principal goes into spec.access.adminPrincipals unless
// it is there, or listed in accessEntries, or the cluster cannot have
// access entries. Without AWS credentials it warns: the platform's role is
// then the only admin.
func addSelfAdmin(ctx context.Context, o Options, c *cloudv1.Cluster) {
	spec := c.GetSpec()
	if o.NoSelfAdmin || spec.GetAws() == nil {
		return
	}
	if spec.GetAws().GetAccessConfig().GetAuthenticationMode() == cloudv1.AwsAuthenticationMode_AWS_AUTHENTICATION_MODE_CONFIG_MAP {
		o.progressf("Warning: authenticationMode CONFIG_MAP has no access entries: you are not made an admin of the cluster\n")
		return
	}
	lookup := o.CallerPrincipal
	if lookup == nil {
		lookup = awsCallerPrincipal
	}
	principal, note, err := lookup(ctx, spec.GetRegion())
	if err != nil {
		o.progressf("Warning: cannot tell who you are in AWS (%v).\n"+
			"  The cluster will have no human admin: only the platform's role can use it.\n"+
			"  Add an admin with spec.access.adminPrincipals (an IAM role or user ARN) and cluster update;\n"+
			"  --no-self-admin silences this warning.\n", err)
		return
	}
	if note != "" {
		o.progressf("Note: %s\n", note)
	}
	if spec.Access == nil {
		spec.Access = &cloudv1.AccessSpec{}
	}
	if slices.Contains(spec.Access.GetAdminPrincipals(), principal) {
		return
	}
	for _, e := range spec.Access.GetAccessEntries() {
		if e.GetPrincipalArn() == principal {
			o.progressf("You (%s) are in spec.access.accessEntries: not added as an admin\n", principal)
			return
		}
	}
	spec.Access.AdminPrincipals = append(spec.Access.AdminPrincipals, principal)
	o.progressf("Adding you (%s) to spec.access.adminPrincipals; --no-self-admin skips this\n", principal)
}
