package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
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
	roleTemplate    = "Resources: {}\n"
)

// accountAPI is a TenantService: the onboarding of a role, the account's
// bootstrap, and an access check that ends with verdict.
type accountAPI struct {
	cloudv1connect.TenantServiceClient
	console      string
	onboarded    *cloudv1.GetAccountOnboardingRequest
	bootstrapped *cloudv1.BootstrapTenantRequest
	verified     bool
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
	// externalID is the onboarded account's: GetAccountOnboarding of the
	// account (its id) answers with it.
	externalID string
}

func (f *accountAPI) DeleteCloudAccount(_ context.Context, _ *cloudv1.DeleteCloudAccountRequest) (*cloudv1.DeleteCloudAccountResponse, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.deleted = true
	return &cloudv1.DeleteCloudAccountResponse{Account: f.account()}, nil
}

func (f *accountAPI) GetAccountOnboarding(_ context.Context, req *cloudv1.GetAccountOnboardingRequest) (*cloudv1.GetAccountOnboardingResponse, error) {
	f.onboarded = req
	role, name := req.GetRole(), req.GetRoleName()
	if name == "" {
		name = "cluster-vending-" + role
	}
	return &cloudv1.GetAccountOnboardingResponse{Onboarding: &cloudv1.AccountOnboarding{Role: role,
		RoleName: name, StackName: name, Region: "us-east-1",
		AccountId: req.GetAccountId(), ManagementRoleArn: managementRole, TemplateBody: roleTemplate,
		TemplateFile: "cluster-vending-" + role + "-role.template.yaml", ConsoleUrl: f.console,
		ExternalId: map[bool]string{true: f.externalID}[req.GetId() != ""]}}, nil
}

func (f *accountAPI) BootstrapTenant(_ context.Context, req *cloudv1.BootstrapTenantRequest) (*cloudv1.BootstrapTenantResponse, error) {
	f.bootstrapped = req
	return &cloudv1.BootstrapTenantResponse{Account: f.account()}, nil
}

func (f *accountAPI) VerifyCloudAccount(_ context.Context, _ *cloudv1.VerifyCloudAccountRequest) (*cloudv1.VerifyCloudAccountResponse, error) {
	f.verified = true
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

// fakeStacks is the tenant account's CloudFormation as the caller's AWS
// credentials see it.
type fakeStacks struct {
	caller string
	// states are what Stack returns, one per call; the last one repeats.
	// A nil entry: no stack.
	states  []*StackInfo
	asked   int
	body    string
	events  []StackEvent
	created []StackInput
	updated []StackInput
	deleted []string
	// unchanged: UpdateStack finds nothing to update.
	unchanged bool
}

func (f *fakeStacks) CallerAccount(context.Context) (string, error) { return f.caller, nil }

func (f *fakeStacks) Stack(context.Context, string) (*StackInfo, error) {
	i := min(f.asked, len(f.states)-1)
	f.asked++
	if i < 0 {
		return nil, nil
	}
	return f.states[i], nil
}

func (f *fakeStacks) TemplateBody(context.Context, string) (string, error) { return f.body, nil }

func (f *fakeStacks) CreateStack(_ context.Context, in StackInput) error {
	f.created = append(f.created, in)
	return nil
}

func (f *fakeStacks) DeleteStack(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeStacks) UpdateStack(_ context.Context, in StackInput) (bool, error) {
	f.updated = append(f.updated, in)
	return !f.unchanged, nil
}

// StackEvents returns, newest first, the events of the states asked so
// far: what a stack reports while it is being created.
func (f *fakeStacks) StackEvents(context.Context, string) ([]StackEvent, error) {
	n := min(f.asked, len(f.events))
	out := make([]StackEvent, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, f.events[i])
	}
	return out, nil
}

func stack(status string) *StackInfo {
	return &StackInfo{Status: status, ManagementRoleARN: managementRole}
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
	stacks   *fakeStacks
	opened   []string
	out      string
	progress string
	err      error
}

// createAccount runs account create with o, against api and, as the
// caller's AWS credentials, stacks (nil: there are none).
func createAccount(t *testing.T, o Options, api *accountAPI, stacks *fakeStacks) run {
	t.Helper()
	r := run{api: api, stacks: stacks}
	var out, progress bytes.Buffer
	o.Progress, o.PollInterval = &progress, time.Nanosecond
	if o.Timeout == 0 {
		o.Timeout = DefaultWaitTimeout
	}
	o.RoleStacks = func(context.Context, string, string) (RoleStacks, error) {
		if stacks == nil {
			return nil, errors.New("no AWS credentials")
		}
		return stacks, nil
	}
	o.OpenURL = func(_ context.Context, url string) error {
		r.opened = append(r.opened, url)
		return nil
	}
	r.err = Run(t.Context(), AccountCreate, Clients{Tenant: api}, o, &out, OutputTable)
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

// From the terminal: the stack is created with the caller's credentials,
// its events are shown as they come, then the account is onboarded and
// its access checked.
func TestAccountCreateFromTheTerminal(t *testing.T) {
	api := &accountAPI{verdict: verified()}
	stacks := &fakeStacks{caller: tenantAccount,
		states: []*StackInfo{nil, stack("CREATE_IN_PROGRESS"), stack("CREATE_COMPLETE")},
		events: []StackEvent{
			{ID: "1", Resource: "cluster-vending-provisioner", Type: "AWS::CloudFormation::Stack", Status: "CREATE_IN_PROGRESS"},
			{ID: "2", Resource: "Role", Type: "AWS::IAM::Role", Status: "CREATE_COMPLETE"},
			{ID: "3", Resource: "cluster-vending-provisioner", Type: "AWS::CloudFormation::Stack", Status: "CREATE_COMPLETE"},
		}}

	r := createAccount(t, Options{Input: manifest(t, "cluster-vending-provisioner"), CreateRole: "terminal", Wait: true}, api, stacks)

	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.progress)
	}
	if len(stacks.created) != 1 || stacks.created[0].Body != roleTemplate || stacks.created[0].ManagementRoleARN != managementRole ||
		stacks.created[0].Name != "cluster-vending-provisioner" {
		t.Errorf("created = %+v", stacks.created)
	}
	if api.onboarded.GetRole() != "provisioner" || api.onboarded.GetAccountId() != tenantAccount {
		t.Errorf("onboarding asked = %v", api.onboarded)
	}
	if api.bootstrapped == nil || !api.verified {
		t.Errorf("bootstrapped %v, verified %v", api.bootstrapped, api.verified)
	}
	contains(t, "progress", r.progress,
		"Creating the stack cluster-vending-provisioner in account "+tenantAccount,
		"Role   AWS::IAM::Role   CREATE_COMPLETE",
		"Stack cluster-vending-provisioner: CREATE_COMPLETE",
		"Access: verified")
	contains(t, "output", r.out, "bootstrapped", "cluster-vending network create")
	t.Logf("stderr:\n%s\nstdout:\n%s", r.progress, r.out)
}

// The caller's credentials are for another account: nothing is created,
// in AWS or on the platform.
func TestAccountCreateWithCredentialsOfAnotherAccount(t *testing.T) {
	api := &accountAPI{}
	stacks := &fakeStacks{caller: platformAccount}

	r := createAccount(t, Options{Input: manifest(t, "cluster-vending-provisioner"), CreateRole: "terminal"}, api, stacks)

	if r.err == nil || !strings.Contains(r.err.Error(), platformAccount) || !strings.Contains(r.err.Error(), "--profile") {
		t.Fatalf("err = %v", r.err)
	}
	if len(stacks.created) > 0 || api.bootstrapped != nil {
		t.Errorf("created %v, bootstrapped %v", stacks.created, api.bootstrapped)
	}
}

// A stack that already creates the role with this template is left alone;
// one with another template is updated.
func TestAccountCreateWithTheStackThere(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		states      []*StackInfo
		unchanged   bool
		wantUpdates int
		want        string
	}{
		{name: "same template", body: roleTemplate, states: []*StackInfo{stack("CREATE_COMPLETE")},
			want: "already creates the role cluster-vending-provisioner: nothing to do"},
		{name: "another template", body: "Resources: {Old: {}}\n",
			states:      []*StackInfo{stack("CREATE_COMPLETE"), stack("UPDATE_IN_PROGRESS"), stack("UPDATE_COMPLETE")},
			wantUpdates: 1, want: "Stack cluster-vending-provisioner: UPDATE_COMPLETE"},
		{name: "another template, nothing to update", body: "Resources: {} # older\n", states: []*StackInfo{stack("UPDATE_COMPLETE")},
			unchanged: true, wantUpdates: 1, want: "nothing to update"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &accountAPI{}
			stacks := &fakeStacks{caller: tenantAccount, states: tt.states, body: tt.body, unchanged: tt.unchanged}

			r := createAccount(t, Options{Input: manifest(t, "cluster-vending-provisioner"), CreateRole: "terminal"}, api, stacks)

			if r.err != nil {
				t.Fatalf("%v\n%s", r.err, r.progress)
			}
			if len(stacks.created) > 0 || len(stacks.updated) != tt.wantUpdates {
				t.Errorf("created %v, updated %v", stacks.created, stacks.updated)
			}
			if api.bootstrapped == nil {
				t.Error("the account was not onboarded")
			}
			contains(t, "progress", r.progress, tt.want)
		})
	}
}

// A stack that fails says why; the account, onboarded first (it has the
// external ID the stack needs), stays, and the error says how to retry
// or remove it.
func TestAccountCreateWhenTheStackFails(t *testing.T) {
	api := &accountAPI{}
	stacks := &fakeStacks{caller: tenantAccount,
		states: []*StackInfo{nil, stack("CREATE_IN_PROGRESS"), stack("ROLLBACK_COMPLETE")},
		events: []StackEvent{
			{ID: "1", Resource: "cluster-vending-provisioner", Type: "AWS::CloudFormation::Stack", Status: "CREATE_IN_PROGRESS"},
			{ID: "2", Resource: "Role", Type: "AWS::IAM::Role", Status: "CREATE_FAILED",
				Reason: "cluster-vending-provisioner already exists in stack other"},
			{ID: "3", Resource: "cluster-vending-provisioner", Type: "AWS::CloudFormation::Stack", Status: "ROLLBACK_COMPLETE"},
		}}

	r := createAccount(t, Options{Input: manifest(t, "cluster-vending-provisioner"), CreateRole: "terminal"}, api, stacks)

	if r.err == nil || !strings.Contains(r.err.Error(), "ROLLBACK_COMPLETE") ||
		!strings.Contains(r.err.Error(), "Role: cluster-vending-provisioner already exists in stack other") {
		t.Fatalf("err = %v", r.err)
	}
	if api.bootstrapped == nil || !strings.Contains(r.err.Error(), "account delete --id "+accountID) {
		t.Errorf("bootstrapped %v, err %v: want the account onboarded and how to remove it", api.bootstrapped, r.err)
	}
}

// The role's stack is created with the account's external ID, which
// GetAccountOnboarding gives for the onboarded account, and tagged with
// its fingerprint; a stack made without it (or before external IDs) is
// updated.
func TestAccountCreatePassesTheExternalID(t *testing.T) {
	ext := strings.Repeat("e", 40)
	tests := []struct {
		name         string
		states       []*StackInfo
		created, upd int
	}{
		{name: "a new stack", states: []*StackInfo{nil, stack("CREATE_COMPLETE")}, created: 1},
		{name: "a stack without it", states: []*StackInfo{stack("CREATE_COMPLETE"), stack("UPDATE_COMPLETE")}, upd: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &accountAPI{externalID: ext}
			stacks := &fakeStacks{caller: tenantAccount, states: tt.states, body: roleTemplate}

			r := createAccount(t, Options{Input: manifest(t, "cluster-vending-provisioner"), CreateRole: "terminal"}, api, stacks)

			if r.err != nil {
				t.Fatalf("%v\n%s", r.err, r.progress)
			}
			if api.onboarded.GetId() != accountID {
				t.Errorf("onboarding asked for %q, want the onboarded account %s", api.onboarded.GetId(), accountID)
			}
			if len(stacks.created) != tt.created || len(stacks.updated) != tt.upd {
				t.Fatalf("created %d, updated %d; want %d, %d", len(stacks.created), len(stacks.updated), tt.created, tt.upd)
			}
			in := append(stacks.created, stacks.updated...)[0]
			if in.ExternalID != ext || in.Tags[cloudformation.ExternalIDTag] != cloudformation.ExternalIDFingerprint(ext) {
				t.Errorf("stack input = %+v, want the external ID and its fingerprint tag", in)
			}
			if strings.Contains(r.progress+r.out, ext) {
				t.Error("the external ID was printed")
			}
		})
	}
}

// Asked nothing: not at a terminal, the way must be given.
func TestAccountCreateNotAtATerminal(t *testing.T) {
	api := &accountAPI{}

	r := createAccount(t, Options{Input: manifest(t, "cluster-vending-provisioner")}, api, &fakeStacks{caller: tenantAccount})

	if r.err == nil || !strings.Contains(r.err.Error(), "--create-role") {
		t.Fatalf("err = %v", r.err)
	}
	if api.onboarded != nil || api.bootstrapped != nil {
		t.Error("something was done")
	}
}

// At a terminal the one question is how to create the role; the browser
// needs the console link, and without one the terminal is used.
func TestAccountCreateAsksBrowserOrTerminal(t *testing.T) {
	api := &accountAPI{}
	stacks := &fakeStacks{caller: tenantAccount, states: []*StackInfo{nil, stack("CREATE_COMPLETE")}}

	r := createAccount(t, Options{Input: manifest(t, "cluster-vending-provisioner"), Terminal: true, In: strings.NewReader("2\n")},
		api, stacks)

	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.progress)
	}
	contains(t, "progress", r.progress, "1) terminal", "2) browser", "3) skip", "cloudformation-template-url", "from the terminal instead")
	if len(r.opened) > 0 || len(stacks.created) != 1 || api.bootstrapped == nil {
		t.Errorf("opened %v, created %v, bootstrapped %v", r.opened, stacks.created, api.bootstrapped)
	}
}

// The browser opens the console's quick-create link; with credentials of
// the account the stack is followed until it is created.
func TestAccountCreateInTheBrowser(t *testing.T) {
	const console = "https://console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/quickcreate?templateURL=x"
	tests := []struct {
		name   string
		stacks *fakeStacks
		want   string
	}{
		{name: "followed", want: "Stack cluster-vending-provisioner: CREATE_COMPLETE",
			stacks: &fakeStacks{caller: tenantAccount, states: []*StackInfo{nil, nil, stack("CREATE_IN_PROGRESS"), stack("CREATE_COMPLETE")}}},
		{name: "no credentials here", want: "not following the stack"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &accountAPI{console: console}

			r := createAccount(t, Options{Input: manifest(t, "cluster-vending-provisioner"), CreateRole: "browser"}, api, tt.stacks)

			if r.err != nil {
				t.Fatalf("%v\n%s", r.err, r.progress)
			}
			if len(r.opened) != 1 || r.opened[0] != console || api.bootstrapped == nil {
				t.Errorf("opened %v, bootstrapped %v", r.opened, api.bootstrapped)
			}
			contains(t, "progress", r.progress, console, tt.want)
			if tt.stacks != nil && len(tt.stacks.created) > 0 {
				t.Errorf("the CLI created the stack itself: %v", tt.stacks.created)
			}
		})
	}
}

// A role the templates do not create, or --create-role skip: no stack.
func TestAccountCreateWithoutAStack(t *testing.T) {
	tests := []struct {
		name, role, mode, want string
	}{
		{name: "another role", role: "acme-admin", mode: "terminal", want: "acme-admin is not a role the platform's templates create"},
		{name: "skip", role: "cluster-vending-provisioner", mode: "skip", want: "Not creating the role cluster-vending-provisioner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &accountAPI{}
			stacks := &fakeStacks{caller: tenantAccount}

			r := createAccount(t, Options{Input: manifest(t, tt.role), CreateRole: tt.mode}, api, stacks)

			if r.err != nil {
				t.Fatal(r.err)
			}
			if api.onboarded != nil || stacks.asked > 0 || api.bootstrapped == nil {
				t.Errorf("onboarded %v, stack asked %d times, bootstrapped %v", api.onboarded, stacks.asked, api.bootstrapped)
			}
			contains(t, "progress", r.progress, tt.want)
		})
	}
}

// A denied access check fails the command, after printing the account.
func TestAccountCreateDenied(t *testing.T) {
	api := &accountAPI{verdict: &cloudv1.AccountAccess{CheckedTime: timestamppb.Now(), Reason: "AssumeRoleFailed",
		Message: "the role does not trust the platform's management role"}}

	r := createAccount(t, Options{Input: manifest(t, "cluster-vending-provisioner"), CreateRole: "skip", Wait: true}, api, nil)

	if r.err == nil || !strings.Contains(r.err.Error(), "AssumeRoleFailed: the role does not trust") ||
		!strings.Contains(r.err.Error(), "account verify --id "+accountID) {
		t.Fatalf("err = %v", r.err)
	}
	contains(t, "output", r.out, "bootstrapped")
}

// Without --wait the account is onboarded and its check left to verify.
func TestAccountCreateWithoutWaiting(t *testing.T) {
	api := &accountAPI{verdict: verified()}

	r := createAccount(t, Options{Input: manifest(t, "cluster-vending-provisioner"), CreateRole: "skip"}, api, nil)

	if r.err != nil || api.verified {
		t.Fatalf("err %v, verified %v", r.err, api.verified)
	}
	contains(t, "output", r.out, "cluster-vending account verify --id "+accountID+" --tenant acme")
}
