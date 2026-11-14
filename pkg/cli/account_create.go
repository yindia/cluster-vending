package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The ways account create creates the platform's role in the AWS account
// (--create-role).
const (
	CreateRoleTerminal = "terminal"
	CreateRoleBrowser  = "browser"
	CreateRoleSkip     = "skip"
)

// AccountCreate onboards the account of a CloudAccount manifest (-f):
//
//  1. When its role is one of the platform's (cluster-vending-reader,
//     -updater, -provisioner), it creates the role's CloudFormation stack
//     in the account: from the terminal with the caller's AWS
//     credentials, or in the browser (the console's quick-create link);
//     --create-role says which, or the one question asked at a terminal.
//  2. TenantService.BootstrapTenant.
//  3. With --wait: the access check (VerifyCloudAccount), until the
//     controller gives its verdict.
//
// A stack that fails stops it before the account is onboarded.
func AccountCreate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadManifest(o.Input, KindCloudAccount)
	if err != nil {
		return nil, err
	}
	tenant, err := m.tenantFor(o)
	if err != nil {
		return nil, err
	}
	account, err := m.CloudAccount()
	if err != nil {
		return nil, err
	}
	if aws := account.GetAws(); aws != nil {
		if err := createRole(ctx, c, o, tenant, aws); err != nil {
			return nil, err
		}
	}
	res, err := call(ctx, &cloudv1.BootstrapTenantRequest{TenantId: tenant, Account: account}, c.Tenant.BootstrapTenant)
	if err != nil {
		return nil, err
	}
	created := res.(*cloudv1.BootstrapTenantResponse)
	if !o.Wait || created.GetAccount().GetAws() == nil {
		return created, nil
	}
	checked, err := checkAccess(ctx, c, o, created.GetAccount())
	if err != nil {
		return nil, err
	}
	out := &cloudv1.BootstrapTenantResponse{Account: checked}
	if access := checked.GetStatus().GetAccess(); access != nil && !access.GetVerified() {
		if err := Print(o.Out, out, o.Output); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("the access check failed: %s: %s; fix the role in account %s, then run: cluster-vending account verify --id %s --tenant %s",
			access.GetReason(), access.GetMessage(), checked.GetAws().GetAccountId(), checked.GetId(), checked.GetTenantId())
	}
	return out, nil
}

// createRole creates the account's role with CloudFormation, the way
// --create-role (or the question) says.
func createRole(ctx context.Context, c Clients, o Options, tenant string, aws *cloudv1.AwsAccount) error {
	role := cloudformation.RoleOf(aws.GetRole())
	if role == "" {
		o.progressf("%s is not a role the platform's templates create: it is yours to create in account %s, trusting the platform's management role. Not creating a stack.\n",
			aws.GetRole(), aws.GetAccountId())
		return nil
	}
	mode, err := chooseCreateRole(o, aws)
	if err != nil {
		return err
	}
	if mode == CreateRoleSkip {
		o.progressf("Not creating the role %s: it is expected to exist in account %s already\n", aws.GetRole(), aws.GetAccountId())
		return nil
	}
	res, err := call(ctx, &cloudv1.GetAccountOnboardingRequest{TenantId: tenant, Role: role, Region: o.StackRegion,
		AccountId: aws.GetAccountId(), RoleName: aws.GetRole()}, c.Tenant.GetAccountOnboarding)
	if err != nil {
		return err
	}
	ob := res.(*cloudv1.GetAccountOnboardingResponse).GetOnboarding()
	if mode == CreateRoleBrowser {
		return createRoleInBrowser(ctx, o, ob, aws.GetAccountId())
	}
	return createRoleFromTerminal(ctx, o, ob, aws.GetAccountId())
}

// chooseCreateRole is --create-role, or the answer to the one question
// account create asks at a terminal.
func chooseCreateRole(o Options, aws *cloudv1.AwsAccount) (string, error) {
	switch o.CreateRole {
	case CreateRoleTerminal, CreateRoleBrowser, CreateRoleSkip:
		return o.CreateRole, nil
	case "":
	default:
		return "", fmt.Errorf("--create-role %q: want terminal, browser or skip", o.CreateRole)
	}
	if !o.Terminal {
		return "", fmt.Errorf("how to create the role %s in account %s: pass --create-role terminal (your AWS credentials), browser (the CloudFormation console) or skip (it exists)",
			aws.GetRole(), aws.GetAccountId())
	}
	o.progressf("Create the IAM role %s in account %s with CloudFormation:\n"+
		"  1) terminal: here, with your AWS credentials of the account\n"+
		"  2) browser: in the CloudFormation console\n"+
		"  3) skip: the role exists already\n"+
		"Choose [1]: ", aws.GetRole(), aws.GetAccountId())
	in := o.In
	if in == nil {
		in = os.Stdin
	}
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the answer: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "", "1", CreateRoleTerminal:
		return CreateRoleTerminal, nil
	case "2", CreateRoleBrowser:
		return CreateRoleBrowser, nil
	case "3", CreateRoleSkip:
		return CreateRoleSkip, nil
	default:
		return "", fmt.Errorf("%q: answer 1, 2 or 3", strings.TrimSpace(answer))
	}
}

// accountStacks are the account's stacks through the caller's AWS
// credentials (--profile, else the default chain), checked to be of the
// account.
func accountStacks(ctx context.Context, o Options, region, account string) (RoleStacks, error) {
	open := o.RoleStacks
	if open == nil {
		open = awsRoleStacks
	}
	s, err := open(ctx, o.Profile, region)
	if err != nil {
		return nil, fmt.Errorf("no AWS credentials for account %s: %w (set AWS_PROFILE or pass --profile)", account, err)
	}
	caller, err := s.CallerAccount(ctx)
	if err != nil {
		return nil, fmt.Errorf("no AWS credentials for account %s: %w (set AWS_PROFILE or pass --profile)", account, err)
	}
	if caller != account {
		return nil, fmt.Errorf("your AWS credentials are of account %s, the manifest's account is %s: pass --profile with credentials of %s",
			caller, account, account)
	}
	return s, nil
}

// createRoleFromTerminal creates (or updates) the role's stack with the
// caller's credentials and follows it.
func createRoleFromTerminal(ctx context.Context, o Options, ob *cloudv1.AccountOnboarding, account string) error {
	s, err := accountStacks(ctx, o, ob.GetRegion(), account)
	if err != nil {
		return err
	}
	return ensureStack(ctx, o, s, ob, account)
}

// createRoleInBrowser opens the console's quick-create link and, when the
// caller's credentials reach the account, follows the stack the person
// creates there. Without a link (the server has no
// cloudformation-template-url) it creates the stack from the terminal.
func createRoleInBrowser(ctx context.Context, o Options, ob *cloudv1.AccountOnboarding, account string) error {
	url := ob.GetConsoleUrl()
	if url == "" {
		o.progressf("The browser needs the console's quick-create link, and the API server has no cloudformation-template-url " +
			"(the templates must be on S3 for the console): creating the stack from the terminal instead\n")
		return createRoleFromTerminal(ctx, o, ob, account)
	}
	o.progressf("Opening the CloudFormation console: sign in to account %s, acknowledge that it creates IAM resources and choose Create stack.\n  %s\n",
		account, url)
	open := o.OpenURL
	if open == nil {
		open = openURL
	}
	if err := open(ctx, url); err != nil {
		o.progressf("Could not open a browser (%v): open the link above\n", err)
	}
	s, err := accountStacks(ctx, o, ob.GetRegion(), account)
	if err != nil {
		o.progressf("%v: not following the stack; the access check says whether the role works\n", err)
		return nil
	}
	return awaitStack(ctx, o, s, ob.GetStackName())
}

// checkAccess asks the controller to check the account's access and
// waits for its verdict: a check later than the one the account has now.
func checkAccess(ctx context.Context, c Clients, o Options, a *cloudv1.CloudAccount) (*cloudv1.CloudAccount, error) {
	before := a.GetStatus().GetAccess().GetCheckedTime().AsTime()
	if _, err := call(ctx, &cloudv1.VerifyCloudAccountRequest{TenantId: a.GetTenantId(), Id: a.GetId()}, c.Tenant.VerifyCloudAccount); err != nil {
		return nil, err
	}
	o.progressf("Checking the account's access: the controller assumes %s in account %s\n",
		a.GetAws().GetRole(), a.GetAws().GetAccountId())
	latest := a
	err := poll(ctx, o, "the access check", func() (bool, error) {
		res, err := call(ctx, &cloudv1.GetCloudAccountRequest{TenantId: a.GetTenantId(), Id: a.GetId()}, c.Tenant.GetCloudAccount)
		if err != nil {
			return false, err
		}
		latest = res.(*cloudv1.GetCloudAccountResponse).GetAccount()
		access := latest.GetStatus().GetAccess()
		return access != nil && access.GetCheckedTime().AsTime().After(before), nil
	})
	if err != nil {
		o.progressf("%v: account get --id %s shows the check when it is done\n", err, a.GetId())
		return latest, nil
	}
	if access := latest.GetStatus().GetAccess(); access.GetVerified() {
		o.progressf("Access: verified, as %s\n", access.GetPrincipalArn())
	}
	return latest, nil
}

// accountNext is what account create prints after the account: what to
// create next once its access is verified, else how to have it checked.
func accountNext(w io.Writer, a *cloudv1.CloudAccount) error {
	if a.GetAws() == nil {
		return nil
	}
	ref := fmt.Sprintf("--id %s --tenant %s", a.GetId(), a.GetTenantId())
	var b strings.Builder
	if a.GetStatus().GetAccess().GetVerified() {
		fmt.Fprintf(&b, "\nNext: cluster-vending network create -f <Network manifest with spec.bootstrapId: %s> --tenant %s\n",
			a.GetId(), a.GetTenantId())
	} else {
		fmt.Fprintf(&b, "\nThen: cluster-vending account verify %s   (networks and clusters need its access verified)\n", ref)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
