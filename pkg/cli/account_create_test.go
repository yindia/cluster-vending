package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

// The placeholders of AWS's documentation: the tenant account, and the
// account of the platform's management role.
const (
	tenantAccount   = "444455556666"
	platformAccount = "111122223333"
	accountID       = "0190a1b2-0000-7000-8000-000000000001"
	managementRole  = "arn:aws:iam::" + platformAccount + ":role/platform"
	externalID      = "3f1c9e2a-7b44-4d8e-9a10-5c2b7d6e8f01"
	apiServer       = "http://127.0.0.1:8080"
	demoRole        = "cluster-vending-demo-0115-provisioner"
)

// accountAPI is a TenantService: the onboarding of a role, the account's
// bootstrap, and an access check that ends with verdict.
type accountAPI struct {
	cloudv1connect.TenantServiceClient
	// onboarded are the GetAccountOnboarding requests, in order.
	onboarded    []*cloudv1.GetAccountOnboardingRequest
	bootstrapped *cloudv1.BootstrapTenantRequest
	verified     bool
	verifiedIn   []string // the regions VerifyCloudAccount was asked to check
	onboardErr   error    // what GetAccountOnboarding answers instead
	// serverPrincipal is the server's principal GetAccountOnboarding names.
	serverPrincipal string
	// verdict is the access check's result once verify was asked; nil:
	// the controller never checks.
	verdict *cloudv1.AccountAccess
	// deleted: DeleteCloudAccount was called; gets is how many reads the
	// account survives after it (0: gone at once); deleteErr fails it.
	deleted   bool
	gets      int
	deleteErr error
	// role is the onboarded account's role (account delete reads it).
	role string
}

func (f *accountAPI) DeleteCloudAccount(_ context.Context, _ *cloudv1.DeleteCloudAccountRequest) (*cloudv1.DeleteCloudAccountResponse, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.deleted = true
	return &cloudv1.DeleteCloudAccountResponse{Account: f.account()}, nil
}

func (f *accountAPI) GetAccountOnboarding(_ context.Context, req *cloudv1.GetAccountOnboardingRequest) (*cloudv1.GetAccountOnboardingResponse, error) {
	f.onboarded = append(f.onboarded, req)
	if f.onboardErr != nil {
		return nil, f.onboardErr
	}
	role, name := req.GetRole(), req.GetRoleName()
	if name == "" {
		name = "cluster-vending-" + role
	}
	ob := &cloudv1.AccountOnboarding{Role: role, RoleName: name, AccountId: req.GetAccountId(),
		ManagementRoleArn: managementRole, TemplatePath: "/cloudformation/" + role + ".yaml", ServerPrincipalArn: f.serverPrincipal}
	if req.GetId() != "" {
		ob.ExternalId = externalID
	}
	return &cloudv1.GetAccountOnboardingResponse{Onboarding: ob}, nil
}

func (f *accountAPI) BootstrapTenant(_ context.Context, req *cloudv1.BootstrapTenantRequest) (*cloudv1.BootstrapTenantResponse, error) {
	f.bootstrapped = req
	return &cloudv1.BootstrapTenantResponse{Account: f.account()}, nil
}

func (f *accountAPI) VerifyCloudAccount(_ context.Context, req *cloudv1.VerifyCloudAccountRequest) (*cloudv1.VerifyCloudAccountResponse, error) {
	f.verified, f.verifiedIn = true, req.GetRegions()
	return &cloudv1.VerifyCloudAccountResponse{Account: f.account()}, nil
}

func (f *accountAPI) GetCloudAccount(_ context.Context, _ *cloudv1.GetCloudAccountRequest) (*cloudv1.GetCloudAccountResponse, error) {
	if f.deleted {
		if f.gets <= 0 {
			return nil, connect.NewError(connect.CodeNotFound, "tenant acme has no bootstrapped account")
		}
		f.gets--
	}
	return &cloudv1.GetCloudAccountResponse{Account: f.account()}, nil
}

func (f *accountAPI) account() *cloudv1.CloudAccount {
	aws := f.bootstrapped.GetAccount().GetAws()
	if f.role != "" {
		aws = &cloudv1.AwsAccount{AccountId: tenantAccount, Role: f.role}
	}
	a := &cloudv1.CloudAccount{Id: accountID, TenantId: "acme", AccountName: "acme-prod",
		Provider: &cloudv1.CloudAccount_Aws{Aws: aws},
		Status:   &cloudv1.CloudAccountStatus{State: cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_READY}}
	if f.verified && f.verdict != nil {
		a.Status.Access = f.verdict
	}
	return a
}

// manifest writes a CloudAccount manifest of the tenant account with role.
func manifest(t *testing.T, role string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudaccount.yaml")
	body := "apiVersion: cluster-vending.io/v1\nkind: CloudAccount\nmetadata:\n  name: acme-prod\n  tenant: acme\n" +
		"spec:\n  aws:\n    accountId: \"" + tenantAccount + "\"\n    role: " + role + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type run struct {
	api      *accountAPI
	out      string
	progress string
	err      error
}

// runAccount runs cmd with o against api, printing in format (empty:
// table).
func runAccount(t *testing.T, cmd Command, o Options, api *accountAPI, format string) run {
	t.Helper()
	if format == "" {
		format = OutputTable
	}
	r := run{api: api}
	var out, progress bytes.Buffer
	o.Progress, o.PollInterval = &progress, time.Nanosecond
	if o.Timeout == 0 {
		o.Timeout = DefaultWaitTimeout
	}
	r.err = Run(t.Context(), cmd, Clients{Tenant: api}, o, &out, format)
	r.out, r.progress = out.String(), progress.String()
	return r
}

func verified() *cloudv1.AccountAccess {
	return &cloudv1.AccountAccess{Verified: true, CheckedTime: timestamppb.Now(),
		PrincipalArn: "arn:aws:iam::" + tenantAccount + ":role/cluster-vending-provisioner"}
}

func contains(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, got)
		}
	}
}

// account create onboards the account, then prints how to create its
// role: the template the API server serves, its parameters with the
// account's external ID, the command that creates the stack, and the
// access check to ask for once it is there. It creates nothing in AWS.
func TestAccountCreatePrintsTheRoleSteps(t *testing.T) {
	api := &accountAPI{}

	r := runAccount(t, AccountCreate, Options{Input: manifest(t, "cluster-vending-provisioner"), Server: apiServer + "/"}, api, "")

	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.progress)
	}
	if api.bootstrapped == nil || api.verified {
		t.Errorf("bootstrapped %v, verified %v: want onboarded, not checked", api.bootstrapped, api.verified)
	}
	if len(api.onboarded) != 2 || api.onboarded[0].GetId() != "" || api.onboarded[1].GetId() != accountID ||
		api.onboarded[1].GetRole() != "provisioner" || api.onboarded[1].GetAccountId() != tenantAccount {
		t.Errorf("onboarding asked = %v, want a check before the account and the account's after", api.onboarded)
	}
	url := apiServer + "/cloudformation/provisioner.yaml"
	contains(t, "output", r.out, "bootstrapped",
		"Template:           "+url,
		"RoleName:           cluster-vending-provisioner",
		"ManagementRoleArn:  "+managementRole,
		"ExternalId:         "+externalID,
		"curl -fsSL "+url+" -o cluster-vending-provisioner.yaml",
		"aws cloudformation create-stack --stack-name cluster-vending-provisioner",
		"--template-body file://cluster-vending-provisioner.yaml --capabilities CAPABILITY_NAMED_IAM",
		"ParameterKey=ManagementRoleArn,ParameterValue="+managementRole,
		"ParameterKey=ExternalId,ParameterValue="+externalID,
		"ParameterKey=RoleName,ParameterValue=cluster-vending-provisioner",
		"--region us-east-1",
		"IAM is global",
		"When the stack is CREATE_COMPLETE: cluster-vending account verify --id "+accountID+" --tenant acme --wait",
		"cluster-vending network create")
	t.Logf("stdout:\n%s", r.out)
}

// A role name of its own (cluster-vending-<anything>-<role>) is that
// role's template, as a stack named after it, with RoleName set.
func TestAccountCreateWithARoleOfItsOwn(t *testing.T) {
	api := &accountAPI{}

	r := runAccount(t, AccountCreate, Options{Input: manifest(t, demoRole), Server: apiServer}, api, "")

	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := api.onboarded[len(api.onboarded)-1]; got.GetRole() != "provisioner" || got.GetRoleName() != demoRole {
		t.Errorf("onboarding asked = %v", got)
	}
	contains(t, "output", r.out, "--stack-name "+demoRole, "ParameterKey=RoleName,ParameterValue="+demoRole,
		apiServer+"/cloudformation/provisioner.yaml")
}

// A role none of the templates creates is the tenant's to make: account
// create says what it must trust, and prints no stack.
func TestAccountCreateWithARoleNotOfTheTemplates(t *testing.T) {
	api := &accountAPI{}

	r := runAccount(t, AccountCreate, Options{Input: manifest(t, "acme-platform"), Server: apiServer}, api, "")

	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := api.onboarded[len(api.onboarded)-1]; got.GetRoleName() != "" || got.GetId() != accountID {
		t.Errorf("onboarding asked = %v, want the account's, without the role's name", got)
	}
	contains(t, "output", r.out, "Create the role acme-platform in account "+tenantAccount+" yourself",
		"trusting "+managementRole, "external ID "+externalID, "account verify --id "+accountID)
	if strings.Contains(r.out, "create-stack") {
		t.Errorf("output has a stack:\n%s", r.out)
	}
}

// With -o json or yaml stdout is the account alone; the steps go to
// stderr.
func TestAccountCreateAsDataPrintsTheStepsOnStderr(t *testing.T) {
	r := runAccount(t, AccountCreate, Options{Input: manifest(t, "cluster-vending-provisioner"), Server: apiServer}, &accountAPI{}, OutputJSON)

	if r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.HasPrefix(strings.TrimSpace(r.out), "{") || strings.Contains(r.out, "create-stack") {
		t.Errorf("stdout:\n%s", r.out)
	}
	contains(t, "stderr", r.progress, "aws cloudformation create-stack")
}

// The server cannot onboard the role (no management role configured):
// account create stops before onboarding the account, so nothing is left.
func TestAccountCreateStopsBeforeOnboardingWhenTheServerCannotSay(t *testing.T) {
	api := &accountAPI{onboardErr: connect.NewError(connect.CodeFailedPrecondition,
		"the API server has no management role configured (aws-management-role-arn)")}

	r := runAccount(t, AccountCreate, Options{Input: manifest(t, "cluster-vending-provisioner"), Server: apiServer}, api, "")

	if r.err == nil || !strings.Contains(r.err.Error(), "aws-management-role-arn") || !strings.Contains(r.err.Error(), "Nothing was created") {
		t.Fatalf("err = %v, want the server's reason", r.err)
	}
	if api.bootstrapped != nil {
		t.Errorf("bootstrapped %v: want nothing", api.bootstrapped)
	}
}

// account verify --wait waits for the controller's verdict; a denied one
// exits non-zero, after printing the account.
func TestAccountVerifyWaits(t *testing.T) {
	tests := []struct {
		name    string
		verdict *cloudv1.AccountAccess
		wantErr string
	}{
		{name: "verified", verdict: verified()},
		{name: "denied", verdict: &cloudv1.AccountAccess{CheckedTime: timestamppb.Now(), Reason: "AssumeRoleFailed",
			Message: "the role does not trust the platform's management role"},
			wantErr: "AssumeRoleFailed: the role does not trust"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &accountAPI{verdict: tt.verdict, role: "cluster-vending-provisioner"}

			r := runAccount(t, BootstrapVerify, Options{ID: accountID, Tenant: "acme", Wait: true, Regions: []string{"us-west-1"}}, api, "")

			if tt.wantErr != "" {
				if r.err == nil || !strings.Contains(r.err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", r.err, tt.wantErr)
				}
			} else if r.err != nil {
				t.Fatalf("%v\n%s", r.err, r.progress)
			}
			if !api.verified || len(api.verifiedIn) != 1 || api.verifiedIn[0] != "us-west-1" {
				t.Errorf("verified %v in %v", api.verified, api.verifiedIn)
			}
			contains(t, "output", r.out, "acme-prod")
			if tt.wantErr == "" {
				contains(t, "progress", r.progress, "Access: verified")
				contains(t, "output", r.out, "cluster-vending network create")
			}
		})
	}
}

// Without --wait verify only asks for the check.
func TestAccountVerifyWithoutWaiting(t *testing.T) {
	api := &accountAPI{verdict: verified(), role: "cluster-vending-provisioner"}

	r := runAccount(t, BootstrapVerify, Options{ID: accountID, Tenant: "acme"}, api, "")

	if r.err != nil || !api.verified {
		t.Fatalf("err %v, verified %v", r.err, api.verified)
	}
	contains(t, "output", r.out, "access check requested")
}

// account delete deletes the account only: the role in AWS is the
// tenant's to delete.
func TestAccountDelete(t *testing.T) {
	tests := []struct {
		name         string
		o            Options
		deleteErr    error
		wantDeleted  bool
		wantErr      string
		wantProgress string
	}{
		{name: "waits until it is gone", o: Options{Wait: true}, wantDeleted: true, wantProgress: "Account " + accountID + ": deleted"},
		{name: "without waiting", wantDeleted: true},
		{name: "networks keep the account", o: Options{Wait: true},
			deleteErr: connect.NewError(connect.CodeFailedPrecondition, "networks prod belong to the account"), wantErr: "networks prod"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &accountAPI{role: demoRole, gets: 2, deleteErr: tt.deleteErr}
			o := tt.o
			o.ID, o.Tenant = accountID, "acme"

			r := runAccount(t, AccountDelete, o, api, "")

			if tt.wantErr != "" {
				if r.err == nil || !strings.Contains(r.err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", r.err, tt.wantErr)
				}
			} else if r.err != nil {
				t.Fatalf("%v\n%s", r.err, r.progress)
			}
			if api.deleted != tt.wantDeleted {
				t.Errorf("account deleted = %v, want %v", api.deleted, tt.wantDeleted)
			}
			if tt.wantProgress != "" {
				contains(t, "progress", r.progress, tt.wantProgress)
			}
			if tt.wantDeleted {
				contains(t, "output", r.out, "cloudaccount/acme-prod ("+accountID+") deleted")
			}
		})
	}
}

// A server that reads the results buckets has a principal the role trusts
// too: the stack's ServerPrincipalArn parameter is printed, and left out when
// the server has none.
func TestAccountCreatePrintsTheServerPrincipalWhenThereIsOne(t *testing.T) {
	const principal = "arn:aws:iam::111122223333:role/cluster-vending-api"
	with := runAccount(t, AccountCreate, Options{Input: manifest(t, "cluster-vending-provisioner"), Server: apiServer},
		&accountAPI{serverPrincipal: principal}, "")
	if with.err != nil {
		t.Fatal(with.err)
	}
	contains(t, "output", with.out, "ServerPrincipalArn:  "+principal, "ParameterKey=ServerPrincipalArn,ParameterValue="+principal)

	without := runAccount(t, AccountCreate, Options{Input: manifest(t, "cluster-vending-provisioner"), Server: apiServer}, &accountAPI{}, "")
	if without.err != nil {
		t.Fatal(without.err)
	}
	if strings.Contains(without.out, "ServerPrincipalArn") {
		t.Errorf("a server without a principal prints one:\n%s", without.out)
	}
}
