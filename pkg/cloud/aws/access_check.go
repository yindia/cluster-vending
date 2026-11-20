package aws

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
)

// Actions an access check makes.
const (
	ActionGetCallerIdentity         = "sts:GetCallerIdentity"
	ActionDescribeAvailabilityZones = "ec2:DescribeAvailabilityZones"
	ActionDescribeVpcs              = "ec2:DescribeVpcs"
	ActionCreateVpcDryRun           = "ec2:CreateVpc"
	ActionCreateTagsDryRun          = "ec2:CreateTags"
	ActionListClusters              = "eks:ListClusters"
	ActionGetRole                   = "iam:GetRole"
	// ActionListRoleTags reads the tenant role's template version tag;
	// not one of AccessChecks: a role that cannot read it is not denied.
	ActionListRoleTags = "iam:ListRoleTags"
)

// Reasons of a failed check.
const (
	// ReasonAssumeRoleDenied: the tenant role cannot be assumed (its trust
	// policy, or the platform's own role lacks sts:AssumeRole).
	ReasonAssumeRoleDenied = "AssumeRoleDenied"
	// ReasonDeniedBySCP: an AWS Organizations service control policy
	// denies the call explicitly, often for whole regions.
	ReasonDeniedBySCP = "DeniedBySCP"
	// ReasonNotAllowed: the tenant role's policies do not allow the call.
	ReasonNotAllowed = "NotAllowed"
	// ReasonWrongAccount: the credentials land in another account, or as
	// another principal than the tenant role.
	ReasonWrongAccount = "WrongAccount"
	// ReasonRegionNotEnabled: the region is not enabled in the account.
	ReasonRegionNotEnabled = "RegionNotEnabled"
	// ReasonUnavailable: throttling or the network; transient, not a
	// verdict about the account.
	ReasonUnavailable = "Unavailable"
)

// globalRegion is where the partition's global services (IAM) are
// called, and the region every check includes.
const globalRegion = GlobalRegion

// GlobalRegion is the region of the partition's global services.
const GlobalRegion = "us-east-1"

// AccessCheck is one call of an access check: what it is, how it is made
// without changing anything, and why the platform needs it.
type AccessCheck struct {
	Action, How, NeededFor string
	// Global: made once, in globalRegion; else in every region.
	Global bool
}

// AccessChecks are the calls an access check makes, in order. Every one
// reads or is a DryRun: nothing is created. iam:SimulatePrincipalPolicy is
// not used: it is often not granted and does not evaluate SCPs.
var AccessChecks = []AccessCheck{
	{ActionGetCallerIdentity, "after assuming the tenant role, in the region's STS endpoint (no permission needed)",
		"every call: the providers assume the same role in the same regions", false},
	{ActionDescribeAvailabilityZones, "read", "network layouts and their zones", false},
	{ActionDescribeVpcs, "read (one page)", "network import and the providers' observation", false},
	{ActionCreateVpcDryRun, "DryRun=true: DryRunOperation when allowed, UnauthorizedOperation when not", "creating networks", false},
	{ActionCreateTagsDryRun, "DryRun=true on a resource that does not exist: DryRunOperation or a not-found answer when allowed (the permission is checked first), UnauthorizedOperation when not",
		"taking over existing networks and clusters (the updater role's write)", false},
	{ActionListClusters, "read (one page)", "EKS name checks, import and the providers' clusters", false},
	{ActionGetRole, "read of the tenant role itself, once (IAM is global)", "the IAM roles the platform creates and reads", true},
}

// regionalChecks are the checks after the identity call, in every region.
var regionalChecks = []string{ActionDescribeAvailabilityZones, ActionDescribeVpcs, ActionCreateVpcDryRun, ActionCreateTagsDryRun, ActionListClusters}

// CheckResult is one call's outcome.
type CheckResult struct {
	Action, Region string
	OK             bool
	// Reason is empty when OK.
	Reason, Message string
}

// RegionAccess is whether the tenant role could be used in one region.
type RegionAccess struct {
	Region          string
	OK              bool
	Reason, Message string
}

// AccessReport is what an access check found.
type AccessReport struct {
	// PrincipalARN is who the calls are made as (the assumed tenant role,
	// or the direct credentials' principal).
	PrincipalARN string
	Regions      []RegionAccess
	Checks       []CheckResult
	// Transient: a call failed for a reason that says nothing about the
	// account (throttling, the network); the report is not a verdict.
	Transient bool
	// TemplateVersion is the version of the role template the tenant
	// role's stack was created from (cloudformation.TagTemplateVersion; 0
	// when the role has no such tag: a stack from before versions), when
	// TemplateVersionKnown: the tags could be read. Reading them never
	// fails the check.
	TemplateVersion      int
	TemplateVersionKnown bool
}

// OK reports whether every region and check passed.
func (r AccessReport) OK() bool { return r.Failed() == nil }

// Failed is the first failure, as a check (Action empty: the region's
// identity call); nil when everything passed.
func (r AccessReport) Failed() *CheckResult {
	for _, reg := range r.Regions {
		if !reg.OK {
			return &CheckResult{Region: reg.Region, Reason: reg.Reason, Message: reg.Message}
		}
	}
	for i := range r.Checks {
		if !r.Checks[i].OK {
			return &r.Checks[i]
		}
	}
	return nil
}

// Message says what failed, naming the region and the action; empty when
// everything passed.
func (r AccessReport) Message() string {
	f := r.Failed()
	switch {
	case f == nil:
		return ""
	case f.Action == "":
		return fmt.Sprintf("in %s the tenant role cannot be used (%s): %s", f.Region, f.Reason, f.Message)
	default:
		return fmt.Sprintf("in %s %s fails (%s): %s", f.Region, f.Action, f.Reason, f.Message)
	}
}

// AccessChecker checks that a tenant's account can be reached and acted in.
type AccessChecker interface {
	// CheckAccess runs AccessChecks through access in regions (and
	// globalRegion). It makes no change in the account.
	CheckAccess(ctx context.Context, access Access, regions []string) (AccessReport, error)
}

// Interfaces of the clients an access check uses.
type (
	accessEC2 interface {
		DescribeAvailabilityZones(context.Context, *ec2.DescribeAvailabilityZonesInput, ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error)
		DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
		CreateVpc(context.Context, *ec2.CreateVpcInput, ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error)
		CreateTags(context.Context, *ec2.CreateTagsInput, ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	}
	accessEKS interface {
		ListClusters(context.Context, *eks.ListClustersInput, ...func(*eks.Options)) (*eks.ListClustersOutput, error)
	}
	accessAPIs struct {
		sts STSAPI
		ec2 accessEC2
		eks accessEKS
		iam accessIAM
	}
	accessIAM interface {
		IAMAPI
		ListRoleTags(context.Context, *iam.ListRoleTagsInput, ...func(*iam.Options)) (*iam.ListRoleTagsOutput, error)
	}
)

// CheckAccess implements AccessChecker with the client's credentials and
// the account's tenant role, the way the providers reach the account.
func (c *Client) CheckAccess(ctx context.Context, access Access, regions []string) (AccessReport, error) {
	if _, err := c.TenantFor(access, globalRegion); err != nil {
		return AccessReport{}, err // an invalid access, before any call
	}
	return checkAccess(ctx, access, regions, func(region string) (accessAPIs, error) {
		cfg, err := c.regionConfig(access, region)
		if err != nil {
			return accessAPIs{}, err
		}
		return accessAPIs{sts: sts.NewFromConfig(cfg), ec2: ec2.NewFromConfig(cfg), eks: eks.NewFromConfig(cfg),
			iam: iam.NewFromConfig(cfg)}, nil
	}, time.Sleep), nil
}

// regionTimeout bounds all the checks of one region (each call is bounded
// too, Config.CallTimeout), so that a region that does not answer costs a
// minute or two, not the retries of every call.
const regionTimeout = 90 * time.Second

// retries is how often a transient failure is tried again, and backoff
// the first wait (doubled each time).
const (
	retries = 2
	backoff = time.Second
)

// checkAccess runs the checks with the clients apis gives per region.
func checkAccess(ctx context.Context, access Access, regions []string, apis func(region string) (accessAPIs, error),
	sleep func(time.Duration)) AccessReport {
	var report AccessReport
	for _, region := range checkedRegions(regions) {
		checkRegion(ctx, &report, access, region, apis, sleep)
	}
	for _, c := range report.Checks {
		report.Transient = report.Transient || c.Reason == ReasonUnavailable
	}
	return report
}

// checkRegion adds the checks of one region to report, within regionTimeout.
func checkRegion(ctx context.Context, report *AccessReport, access Access, region string,
	apis func(region string) (accessAPIs, error), sleep func(time.Duration)) {
	ctx, cancel := context.WithTimeout(ctx, regionTimeout)
	defer cancel()
	api, err := apis(region)
	if err != nil {
		report.Regions = append(report.Regions, RegionAccess{Region: region, Reason: ReasonNotAllowed, Message: err.Error()})
		return
	}
	regional := RegionAccess{Region: region, OK: true}
	var out *sts.GetCallerIdentityOutput
	err = retry(ctx, sleep, func() error {
		var err error
		out, err = api.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		return err
	})
	if err != nil {
		regional.OK, regional.Reason, regional.Message = false, identityReason(err), errorText(err)
		report.Transient = report.Transient || regional.Reason == ReasonUnavailable
		report.Regions = append(report.Regions, regional)
		return
	}
	if reason, msg := principalProblem(access, out); reason != "" {
		regional.OK, regional.Reason, regional.Message = false, reason, msg
		report.Regions = append(report.Regions, regional)
		return
	}
	report.PrincipalARN = awssdk.ToString(out.Arn)
	report.Regions = append(report.Regions, regional)
	for _, action := range regionalChecks {
		report.Checks = append(report.Checks, run(ctx, sleep, action, region, func() error { return call(ctx, api, action) }))
	}
	if region == globalRegion && !access.Direct {
		name := access.RoleARN[strings.LastIndex(access.RoleARN, "/")+1:]
		report.Checks = append(report.Checks, run(ctx, sleep, ActionGetRole, region, func() error {
			_, err := api.iam.GetRole(ctx, &iam.GetRoleInput{RoleName: awssdk.String(name)})
			return err
		}))
		readTemplateVersion(ctx, report, api.iam, name, sleep)
	}
}

// readTemplateVersion reads the template version tag of the tenant role
// (once: IAM is global). A role whose tags cannot be read leaves the
// version unknown; only a transient failure is noted, for the check to be
// tried again.
func readTemplateVersion(ctx context.Context, report *AccessReport, api accessIAM, roleName string, sleep func(time.Duration)) {
	var out *iam.ListRoleTagsOutput
	err := retry(ctx, sleep, func() error {
		var err error
		out, err = api.ListRoleTags(ctx, &iam.ListRoleTagsInput{RoleName: awssdk.String(roleName)})
		return err
	})
	switch {
	case err == nil:
		tags := make(map[string]string, len(out.Tags))
		for _, t := range out.Tags {
			tags[awssdk.ToString(t.Key)] = awssdk.ToString(t.Value)
		}
		report.TemplateVersion, report.TemplateVersionKnown = cloudformation.VersionOfTags(tags), true
	case callReason(err) == ReasonUnavailable:
		report.Transient = true
	}
}

// checkedRegions are regions and globalRegion, globalRegion first, each
// once, valid ones only.
func checkedRegions(regions []string) []string {
	out := []string{globalRegion}
	for _, r := range regions {
		if r != "" && awscatalog.IsRegion(r) && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.Sort(out[1:])
	return out
}

// call makes one regional check.
func call(ctx context.Context, api accessAPIs, action string) error {
	var err error
	switch action {
	case ActionDescribeAvailabilityZones:
		_, err = api.ec2.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{})
	case ActionDescribeVpcs:
		_, err = api.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{MaxResults: awssdk.Int32(5)})
	case ActionCreateVpcDryRun:
		_, err = api.ec2.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: awssdk.String("10.255.0.0/28"), DryRun: awssdk.Bool(true)})
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "DryRunOperation" {
			err = nil // allowed: the DryRun answer when the call would succeed
		}
	case ActionCreateTagsDryRun:
		_, err = api.ec2.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{"vpc-00000000000000000"},
			Tags: []ec2types.Tag{{Key: awssdk.String("cluster-vending.io/access-check"), Value: awssdk.String("dry-run")}}, DryRun: awssdk.Bool(true)})
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "DryRunOperation" || strings.HasSuffix(apiErr.ErrorCode(), ".NotFound") ||
			strings.HasSuffix(apiErr.ErrorCode(), ".Malformed")) {
			err = nil // allowed: the permission is checked before the resource
		}
	case ActionListClusters:
		_, err = api.eks.ListClusters(ctx, &eks.ListClustersInput{MaxResults: awssdk.Int32(1)})
	}
	return err
}

// run makes one check, retrying transient failures.
func run(ctx context.Context, sleep func(time.Duration), action, region string, f func() error) CheckResult {
	err := retry(ctx, sleep, f)
	if err == nil {
		return CheckResult{Action: action, Region: region, OK: true}
	}
	return CheckResult{Action: action, Region: region, Reason: callReason(err), Message: errorText(err)}
}

// retry calls f again after a transient failure, with backoff.
func retry(ctx context.Context, sleep func(time.Duration), f func() error) error {
	wait := backoff
	for attempt := 0; ; attempt++ {
		err := f()
		if err == nil || callReason(err) != ReasonUnavailable || attempt == retries || ctx.Err() != nil {
			return err
		}
		sleep(wait)
		wait *= 2
	}
}

// principalProblem says why the identity is not the one expected: another
// account, or (through a role) another role than the tenant role.
func principalProblem(access Access, out *sts.GetCallerIdentityOutput) (reason, message string) {
	account, arn := awssdk.ToString(out.Account), awssdk.ToString(out.Arn)
	if account != access.AccountID {
		return ReasonWrongAccount, fmt.Sprintf("the credentials are for account %s (%s), not %s", account, arn, access.AccountID)
	}
	if access.Direct {
		return "", ""
	}
	role := access.RoleARN[strings.LastIndex(access.RoleARN, "/")+1:]
	if !strings.Contains(arn, ":assumed-role/"+role+"/") {
		return ReasonWrongAccount, fmt.Sprintf("the calls are made as %s, not as the tenant role %s", arn, access.RoleARN)
	}
	return "", ""
}

// identityReason is why the identity call (after assuming the role)
// failed: assuming the role is what is denied.
func identityReason(err error) string {
	switch reason := callReason(err); reason {
	case ReasonNotAllowed:
		return ReasonAssumeRoleDenied
	default:
		return reason
	}
}

// callReason classifies an AWS error.
func callReason(err error) string {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return ReasonUnavailable // the network, a timeout: no answer from AWS
	}
	code, msg := api.ErrorCode(), strings.ToLower(api.ErrorMessage())
	switch {
	case strings.Contains(msg, "explicit deny in a service control policy"):
		return ReasonDeniedBySCP
	case code == "OptInRequired", code == "InvalidClientTokenId", code == "UnrecognizedClientException",
		strings.Contains(msg, "region is not enabled"), strings.Contains(msg, "not enabled for this account"):
		return ReasonRegionNotEnabled
	case code == "Throttling", code == "ThrottlingException", code == "RequestLimitExceeded", code == "TooManyRequestsException",
		code == "ServiceUnavailable", code == "InternalError", code == "InternalFailure", code == "RequestExpired":
		return ReasonUnavailable
	default:
		return ReasonNotAllowed
	}
}

// errorText is the AWS message of err, or err itself.
func errorText(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode() + ": " + api.ErrorMessage()
	}
	return err.Error()
}
