package tenant_test

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/tenant"
)

const platformRole = "arn:aws:iam::111122223333:role/platform"

func TestAccountOnboarding(t *testing.T) {
	tests := []struct {
		name    string
		config  tenant.Onboarding
		req     *cloudv1.GetAccountOnboardingRequest
		want    *cloudv1.AccountOnboarding
		console bool
	}{
		{
			name:   "the default role and region, no hosted templates",
			config: tenant.Onboarding{ManagementRoleARN: platformRole},
			req:    &cloudv1.GetAccountOnboardingRequest{TenantId: "acme", AccountId: "444455556666"},
			want: &cloudv1.AccountOnboarding{Role: "provisioner", RoleName: "cluster-vending-provisioner",
				StackName: "cluster-vending-provisioner", Region: "us-east-1", AccountId: "444455556666",
				ManagementRoleArn: platformRole, TemplateFile: "cluster-vending-provisioner-role.template.yaml"},
		},
		{
			name:   "hosted templates: a console link",
			config: tenant.Onboarding{ManagementRoleARN: platformRole, TemplateURL: "https://bucket.s3.amazonaws.com/cv"},
			req:    &cloudv1.GetAccountOnboardingRequest{TenantId: "acme", Role: "reader", Region: "eu-west-1"},
			want: &cloudv1.AccountOnboarding{Role: "reader", RoleName: "cluster-vending-reader",
				StackName: "cluster-vending-reader", Region: "eu-west-1", ManagementRoleArn: platformRole,
				TemplateFile: "cluster-vending-reader-role.template.yaml",
				TemplateUrl:  "https://bucket.s3.amazonaws.com/cv/cluster-vending-reader-role.template.yaml"},
			console: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := tenant.NewTenantService(reconcile.Deps{}, tenant.WithOnboarding(tt.config))

			res, err := svc.GetAccountOnboarding(t.Context(), tt.req)

			if err != nil {
				t.Fatal(err)
			}
			got := res.GetOnboarding()
			if !strings.Contains(got.GetTemplateBody(), "AWS::IAM::Role") {
				t.Errorf("template body = %.80q", got.GetTemplateBody())
			}
			if tt.console != strings.HasPrefix(got.GetConsoleUrl(), "https://console.aws.amazon.com/cloudformation/") {
				t.Errorf("console url = %q", got.GetConsoleUrl())
			}
			got.TemplateBody, got.ConsoleUrl = "", ""
			if got.String() != tt.want.String() {
				t.Errorf("got  %v\nwant %v", got, tt.want)
			}
		})
	}
}

// Without the platform's management role the server cannot say what the
// role trusts: it says so instead of guessing.
func TestAccountOnboardingNeedsTheManagementRole(t *testing.T) {
	svc := tenant.NewTenantService(reconcile.Deps{})

	_, err := svc.GetAccountOnboarding(t.Context(), &cloudv1.GetAccountOnboardingRequest{TenantId: "acme"})

	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition || !strings.Contains(ce.Message(), "aws-management-role-arn") {
		t.Fatalf("err = %v", err)
	}
}

func TestAccountOnboardingValidates(t *testing.T) {
	svc := tenant.NewTenantService(reconcile.Deps{}, tenant.WithOnboarding(tenant.Onboarding{ManagementRoleARN: platformRole}))

	_, err := svc.GetAccountOnboarding(t.Context(), &cloudv1.GetAccountOnboardingRequest{TenantId: "acme", Role: "admin"})

	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("err = %v", err)
	}
}
