package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

// onboardingAPI is a TenantService that answers GetAccountOnboarding and
// GetCloudAccount.
type onboardingAPI struct {
	cloudv1connect.TenantServiceClient
	asked   *cloudv1.GetAccountOnboardingRequest
	console string
}

func (f *onboardingAPI) GetAccountOnboarding(_ context.Context, req *cloudv1.GetAccountOnboardingRequest) (*cloudv1.GetAccountOnboardingResponse, error) {
	f.asked = req
	role := req.GetRole()
	return &cloudv1.GetAccountOnboardingResponse{Onboarding: &cloudv1.AccountOnboarding{Role: role,
		RoleName: "cluster-vending-" + role, StackName: "cluster-vending-" + role, Region: "us-east-1",
		AccountId: req.GetAccountId(), ManagementRoleArn: "arn:aws:iam::111122223333:role/platform",
		TemplateBody: "Resources: {}\n", TemplateFile: "cluster-vending-" + role + "-role.template.yaml",
		ConsoleUrl: f.console}}, nil
}

func (f *onboardingAPI) GetCloudAccount(_ context.Context, req *cloudv1.GetCloudAccountRequest) (*cloudv1.GetCloudAccountResponse, error) {
	return &cloudv1.GetCloudAccountResponse{Account: &cloudv1.CloudAccount{Id: req.GetId(), TenantId: req.GetTenantId(),
		Provider: &cloudv1.CloudAccount_Aws{Aws: &cloudv1.AwsAccount{AccountId: "444455556666", Role: "cluster-vending-updater"}}}}, nil
}

func TestAccountTemplate(t *testing.T) {
	out := filepath.Join(t.TempDir(), "role.yaml")
	tests := []struct {
		name     string
		opts     Options
		console  string
		wantRole string
		want     []string
		absent   []string
	}{
		{
			name:     "the default role, written where asked, no console link",
			opts:     Options{Tenant: "acme", AccountID: "444455556666", TemplateOut: out, Server: "https://api.example.com"},
			wantRole: "provisioner",
			want: []string{
				"Template:   " + out,
				"Download:   https://api.example.com/cloudformation/cluster-vending-provisioner-role.template.yaml",
				"with credentials of account 444455556666",
				"aws cloudformation create-stack --stack-name cluster-vending-provisioner --template-body file://" + out,
				"ParameterKey=ManagementRoleArn,ParameterValue=arn:aws:iam::111122223333:role/platform",
				"cloudformation-template-url",
				"spec.aws.role: cluster-vending-provisioner",
				"cluster-vending account verify --id <id> --tenant acme",
			},
			absent: []string{"Console:"},
		},
		{
			name:     "an onboarded account: its role and account, and the console link",
			opts:     Options{Tenant: "acme", ID: "0190a1b2-0000-7000-8000-000000000001", TemplateOut: out},
			console:  "https://console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/quickcreate?x",
			wantRole: "updater",
			want: []string{
				"Console:    https://console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/quickcreate?x",
				"with credentials of account 444455556666",
				"cluster-vending account verify --id 0190a1b2-0000-7000-8000-000000000001 --tenant acme",
			},
			absent: []string{"cloudformation-template-url", "spec.aws.role"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &onboardingAPI{console: tt.console}
			var buf bytes.Buffer

			err := Run(t.Context(), AccountTemplate, Clients{Tenant: api}, tt.opts, &buf, OutputTable)

			if err != nil {
				t.Fatal(err)
			}
			if api.asked.GetRole() != tt.wantRole {
				t.Errorf("asked for role %q, want %q", api.asked.GetRole(), tt.wantRole)
			}
			for _, w := range tt.want {
				if !strings.Contains(buf.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, buf.String())
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(buf.String(), a) {
					t.Errorf("output has %q:\n%s", a, buf.String())
				}
			}
			if body, err := os.ReadFile(out); err != nil || string(body) != "Resources: {}\n" {
				t.Errorf("template file = %q, %v", body, err)
			}
		})
	}
}

// account create says how to create the role and check it.
func TestAccountCreateSaysWhatIsNext(t *testing.T) {
	account := func(role string) *cloudv1.BootstrapTenantResponse {
		return &cloudv1.BootstrapTenantResponse{Account: &cloudv1.CloudAccount{Id: "0190a1b2-0000-7000-8000-000000000001",
			TenantId: "acme", Provider: &cloudv1.CloudAccount_Aws{Aws: &cloudv1.AwsAccount{AccountId: "444455556666", Role: role}}}}
	}

	got := printed(t, account("cluster-vending-provisioner"), OutputTable)
	other := printed(t, account("acme-admin"), OutputTable)

	for _, want := range []string{
		"cluster-vending account template --id 0190a1b2-0000-7000-8000-000000000001 --tenant acme",
		"cluster-vending account verify --id 0190a1b2-0000-7000-8000-000000000001 --tenant acme",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(other, "acme-admin") || !strings.Contains(other, "--role provisioner") {
		t.Errorf("another role:\n%s", other)
	}
}

// A role name the templates do not create cannot be onboarded with them.
func TestAccountTemplateOfAnotherRole(t *testing.T) {
	api := &onboardingAPI{}
	api.TenantServiceClient = nil

	err := Run(t.Context(), AccountTemplate, Clients{Tenant: &otherRoleAPI{api}},
		Options{Tenant: "acme", ID: "0190a1b2-0000-7000-8000-000000000001"}, &bytes.Buffer{}, OutputTable)

	if err == nil || !strings.Contains(err.Error(), "acme-admin") || !strings.Contains(err.Error(), "--role") {
		t.Fatalf("err = %v", err)
	}
}

type otherRoleAPI struct{ *onboardingAPI }

func (f *otherRoleAPI) GetCloudAccount(_ context.Context, req *cloudv1.GetCloudAccountRequest) (*cloudv1.GetCloudAccountResponse, error) {
	return &cloudv1.GetCloudAccountResponse{Account: &cloudv1.CloudAccount{Id: req.GetId(),
		Provider: &cloudv1.CloudAccount_Aws{Aws: &cloudv1.AwsAccount{AccountId: "444455556666", Role: "acme-admin"}}}}, nil
}
