package aws

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// probeFake answers every call of an access check in one region. An
// entry of errs makes that action fail.
type probeFake struct {
	account, arn string
	errs         map[string]error
	calls        *[]string
	// flaky fails an action with Throttling this many times first.
	flaky map[string]int
}

func (f probeFake) fail(action string) error {
	*f.calls = append(*f.calls, action)
	if f.flaky[action] > 0 {
		f.flaky[action]--
		return apiErr("Throttling", "Rate exceeded")
	}
	return f.errs[action]
}

func (f probeFake) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if err := f.fail(ActionGetCallerIdentity); err != nil {
		return nil, err
	}
	return &sts.GetCallerIdentityOutput{Account: awssdk.String(f.account), Arn: awssdk.String(f.arn)}, nil
}

func (f probeFake) DescribeAvailabilityZones(context.Context, *ec2.DescribeAvailabilityZonesInput, ...func(*ec2.Options)) (*ec2.DescribeAvailabilityZonesOutput, error) {
	return &ec2.DescribeAvailabilityZonesOutput{}, f.fail(ActionDescribeAvailabilityZones)
}

func (f probeFake) DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	return &ec2.DescribeVpcsOutput{}, f.fail(ActionDescribeVpcs)
}

func (f probeFake) CreateVpc(_ context.Context, in *ec2.CreateVpcInput, _ ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error) {
	if !awssdk.ToBool(in.DryRun) {
		panic("CreateVpc without DryRun: the check must never create anything")
	}
	if err := f.fail(ActionCreateVpcDryRun); err != nil {
		return nil, err
	}
	return nil, &smithy.GenericAPIError{Code: "DryRunOperation", Message: "Request would have succeeded, but DryRun flag is set."}
}

func (f probeFake) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	if !awssdk.ToBool(in.DryRun) {
		panic("CreateTags without DryRun: the check must never change anything")
	}
	if err := f.fail(ActionCreateTagsDryRun); err != nil {
		return nil, err
	}
	return nil, &smithy.GenericAPIError{Code: "InvalidVpcID.NotFound", Message: "The vpc ID does not exist"}
}

func (f probeFake) ListClusters(context.Context, *eks.ListClustersInput, ...func(*eks.Options)) (*eks.ListClustersOutput, error) {
	return &eks.ListClustersOutput{}, f.fail(ActionListClusters)
}

func (f probeFake) GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	return &iam.GetRoleOutput{}, f.fail(ActionGetRole)
}

func apiErr(code, message string) error { return &smithy.GenericAPIError{Code: code, Message: message} }

const assumedARN = "arn:aws:sts::444455556666:assumed-role/cluster-vending-provisioner/cluster-vending"

// checkWith runs an access check whose regions answer with the fakes of
// byRegion (a region not listed answers everything).
func checkWith(t *testing.T, a Access, regions []string, byRegion map[string]map[string]error) (AccessReport, []string) {
	t.Helper()
	var calls []string
	report := checkAccess(t.Context(), a, regions, func(region string) (accessAPIs, error) {
		f := probeFake{account: tenantAccount, arn: assumedARN, errs: byRegion[region], calls: &calls}
		if a.Direct {
			f.arn = "arn:aws:iam::444455556666:user/dev"
		}
		return accessAPIs{sts: f, ec2: f, eks: f, iam: f}, nil
	}, func(time.Duration) {})
	return report, calls
}

func TestCheckAccessAllowed(t *testing.T) {
	a := Access{AccountID: tenantAccount, RoleARN: tenantRole}

	report, calls := checkWith(t, a, []string{"us-west-1"}, nil)

	if !report.OK() || report.Transient || report.PrincipalARN != assumedARN {
		t.Fatalf("report = %+v", report)
	}
	var regions []string
	for _, r := range report.Regions {
		regions = append(regions, r.Region)
	}
	if !slices.Equal(regions, []string{"us-east-1", "us-west-1"}) {
		t.Errorf("regions = %v; us-east-1 (global IAM, the partition's STS) always comes first", regions)
	}
	if n := strings.Count(strings.Join(calls, ","), ActionGetRole); n != 1 {
		t.Errorf("GetRole called %d times; IAM is global, once is enough", n)
	}
	for _, c := range report.Checks {
		if !c.OK {
			t.Errorf("check %+v failed", c)
		}
	}
}

func TestCheckAccessReasons(t *testing.T) {
	scp := apiErr("AccessDenied", "User: arn:aws:sts::444455556666:assumed-role/x/y is not authorized to perform: ec2:CreateVpc "+
		"with an explicit deny in a service control policy")
	tests := []struct {
		name      string
		access    Access
		regions   []string
		errs      map[string]map[string]error
		region    string // the region that fails
		action    string // the action that fails; empty: the region itself
		reason    string
		transient bool
	}{
		{name: "assume role denied in a region (SCP on sts)", regions: []string{"us-west-1"},
			errs: map[string]map[string]error{"us-west-1": {ActionGetCallerIdentity: apiErr("AccessDenied",
				"User is not authorized to perform: sts:AssumeRole with an explicit deny in a service control policy")}},
			region: "us-west-1", reason: ReasonDeniedBySCP},
		{name: "assume role refused (trust policy)", regions: []string{"us-west-1"},
			errs:   map[string]map[string]error{"us-west-1": {ActionGetCallerIdentity: apiErr("AccessDenied", "not authorized to perform: sts:AssumeRole")}},
			region: "us-west-1", reason: ReasonAssumeRoleDenied},
		{name: "region not enabled", regions: []string{"ap-east-1"},
			errs: map[string]map[string]error{"ap-east-1": {ActionGetCallerIdentity: apiErr("InvalidClientTokenId",
				"The security token included in the request is invalid")}},
			region: "ap-east-1", reason: ReasonRegionNotEnabled},
		{name: "a dry-run tag denied", regions: []string{"us-west-1"},
			errs:   map[string]map[string]error{"us-west-1": {ActionCreateTagsDryRun: apiErr("UnauthorizedOperation", "You are not authorized")}},
			region: "us-west-1", action: ActionCreateTagsDryRun, reason: ReasonNotAllowed},
		{name: "a dry-run create denied by an SCP", regions: []string{"us-west-1"},
			errs:   map[string]map[string]error{"us-west-1": {ActionCreateVpcDryRun: scp}},
			region: "us-west-1", action: ActionCreateVpcDryRun, reason: ReasonDeniedBySCP},
		{name: "a dry-run create not allowed", regions: []string{"us-west-1"},
			errs:   map[string]map[string]error{"us-west-1": {ActionCreateVpcDryRun: apiErr("UnauthorizedOperation", "You are not authorized")}},
			region: "us-west-1", action: ActionCreateVpcDryRun, reason: ReasonNotAllowed},
		{name: "eks not allowed", regions: []string{"us-west-1"},
			errs:   map[string]map[string]error{"us-west-1": {ActionListClusters: apiErr("AccessDeniedException", "not authorized")}},
			region: "us-west-1", action: ActionListClusters, reason: ReasonNotAllowed},
		{name: "throttled is transient", regions: []string{"us-west-1"},
			errs:   map[string]map[string]error{"us-west-1": {ActionDescribeVpcs: apiErr("RequestLimitExceeded", "slow down")}},
			region: "us-west-1", action: ActionDescribeVpcs, reason: ReasonUnavailable, transient: true},
		{name: "a network error is transient", regions: []string{"us-west-1"},
			errs:   map[string]map[string]error{"us-west-1": {ActionGetCallerIdentity: errors.New("dial tcp: lookup sts.us-west-1.amazonaws.com: no such host")}},
			region: "us-west-1", reason: ReasonUnavailable, transient: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.access
			if a.AccountID == "" {
				a = Access{AccountID: tenantAccount, RoleARN: tenantRole}
			}

			report, _ := checkWith(t, a, tt.regions, tt.errs)

			if report.OK() || report.Transient != tt.transient {
				t.Fatalf("report = %+v, want a failure (transient %v)", report, tt.transient)
			}
			failed := report.Failed()
			if failed == nil || failed.Region != tt.region || failed.Action != tt.action || failed.Reason != tt.reason {
				t.Fatalf("first failure = %+v, want %s %q %s", failed, tt.region, tt.action, tt.reason)
			}
			if !strings.Contains(report.Message(), tt.region) {
				t.Errorf("message %q does not name the region", report.Message())
			}
		})
	}
}

// A region whose identity call fails runs none of its other checks.
func TestCheckAccessStopsInARegionWithoutIdentity(t *testing.T) {
	a := Access{AccountID: tenantAccount, RoleARN: tenantRole}

	_, calls := checkWith(t, a, []string{"us-west-1"},
		map[string]map[string]error{"us-west-1": {ActionGetCallerIdentity: apiErr("AccessDenied", "nope")}})

	west := 0
	for _, c := range calls {
		if c != ActionGetCallerIdentity {
			west++
		}
	}
	// us-east-1 runs its checks; us-west-1 only the identity call.
	if want := len(regionalChecks) + 1; west != want {
		t.Errorf("%d other calls, want %d (us-east-1's only): %v", west, want, calls)
	}
}

func TestCheckAccessWrongAccountOrRole(t *testing.T) {
	tests := []struct {
		name, account, arn, reason string
	}{
		{"another account", "111122223333", "arn:aws:sts::111122223333:assumed-role/cluster-vending-provisioner/s", ReasonWrongAccount},
		{"another role", tenantAccount, "arn:aws:sts::444455556666:assumed-role/admin/s", ReasonWrongAccount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			report := checkAccess(t.Context(), Access{AccountID: tenantAccount, RoleARN: tenantRole}, nil,
				func(string) (accessAPIs, error) {
					f := probeFake{account: tt.account, arn: tt.arn, calls: &calls}
					return accessAPIs{sts: f, ec2: f, eks: f, iam: f}, nil
				}, func(time.Duration) {})

			if report.OK() || report.Failed().Reason != tt.reason {
				t.Fatalf("report = %+v", report)
			}
		})
	}
}

// Direct access (local testing) uses the keys as they are: any principal
// of the account is accepted, and there is no tenant role to read.
func TestCheckAccessDirect(t *testing.T) {
	a := Access{AccountID: tenantAccount, Direct: true}

	report, calls := checkWith(t, a, nil, nil)

	if !report.OK() || report.PrincipalARN != "arn:aws:iam::444455556666:user/dev" {
		t.Fatalf("report = %+v", report)
	}
	if slices.Contains(calls, ActionGetRole) {
		t.Errorf("GetRole called in direct mode: %v", calls)
	}
}

// A transient failure is retried before it is reported.
func TestCheckAccessRetriesTransient(t *testing.T) {
	var calls []string
	flaky := map[string]int{ActionDescribeVpcs: 1}
	report := checkAccess(t.Context(), Access{AccountID: tenantAccount, RoleARN: tenantRole}, nil,
		func(string) (accessAPIs, error) {
			f := probeFake{account: tenantAccount, arn: assumedARN, calls: &calls, flaky: flaky}
			return accessAPIs{sts: f, ec2: f, eks: f, iam: f}, nil
		}, func(time.Duration) {})

	if !report.OK() {
		t.Fatalf("report = %+v, want the retry to succeed", report)
	}
}

func TestAccessChecksTable(t *testing.T) {
	for _, c := range AccessChecks {
		if c.Action == "" || c.How == "" || c.NeededFor == "" {
			t.Errorf("check %+v is not documented", c)
		}
	}
}
