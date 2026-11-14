package tenant

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
	"github.com/yindia/cluster-vending/pkg/config"
	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// Onboarding is what the server tells a tenant about creating the
// platform's role in an AWS account (config.Onboarding).
type Onboarding struct {
	// ManagementRoleARN is the role the tenant role trusts; empty: the
	// server cannot say, and GetAccountOnboarding fails.
	ManagementRoleARN string
	// TemplateURL is the HTTPS location the templates are hosted at (an S3
	// bucket); empty: no console link.
	TemplateURL string
}

// Option configures the TenantService.
type Option func(*TenantService)

// WithOnboarding sets what GetAccountOnboarding answers with.
func WithOnboarding(o Onboarding) Option {
	return func(s *TenantService) { s.onboarding = o }
}

// GetAccountOnboarding answers with the CloudFormation template of the
// role and how to create its stack. It reads nothing and calls no AWS.
func (s *TenantService) GetAccountOnboarding(_ context.Context, req *cloudv1.GetAccountOnboardingRequest) (*cloudv1.GetAccountOnboardingResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if s.onboarding.ManagementRoleARN == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			"the API server has no management role configured ("+config.FlagAWSManagementRoleARN+
				"): the platform team sets it to the role the controller runs as")
	}
	role := req.GetRole()
	if role == "" {
		role = cloudformation.DefaultRole
	}
	region := req.GetRegion()
	if region == "" {
		region = string(awscatalog.USEast1)
	}
	body, err := cloudformation.Template(role)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	out := &cloudv1.AccountOnboarding{Role: role, RoleName: cloudformation.RoleName(role), StackName: cloudformation.RoleName(role),
		Region: region, AccountId: req.GetAccountId(), ManagementRoleArn: s.onboarding.ManagementRoleARN,
		TemplateBody: string(body), TemplateFile: cloudformation.FileName(role)}
	if s.onboarding.TemplateURL != "" {
		stack := cloudformation.Stack{Role: role, Region: region, ManagementRoleARN: s.onboarding.ManagementRoleARN}
		console, err := cloudformation.QuickCreateURL(s.onboarding.TemplateURL, stack)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error())
		}
		out.ConsoleUrl = console
		out.TemplateUrl = cloudformation.TemplateURL(s.onboarding.TemplateURL, role)
	}
	return &cloudv1.GetAccountOnboardingResponse{Onboarding: out}, nil
}
