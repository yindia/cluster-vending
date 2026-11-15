package tenant

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	accountreconcile "github.com/yindia/cluster-vending/pkg/reconcile/account"

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
func (s *TenantService) GetAccountOnboarding(ctx context.Context, req *cloudv1.GetAccountOnboardingRequest) (*cloudv1.GetAccountOnboardingResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	var account *model.CloudAccount
	if req.GetId() != "" {
		a, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, req.GetTenantId(), req.GetId())
		if errors.Is(err, repository.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound,
				fmt.Sprintf("tenant %s has no bootstrapped account %s", req.GetTenantId(), req.GetId()))
		}
		if err != nil {
			return nil, service.ToConnect(err)
		}
		account = a
		if req.GetRoleName() == "" && req.GetRole() == "" {
			req = proto.CloneOf(req)
			req.RoleName = accountreconcile.RoleName(a.Identity)
		}
	}
	if s.onboarding.ManagementRoleARN == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			"the API server has no management role configured ("+config.FlagAWSManagementRoleARN+
				"): the platform team sets it to the role the controller runs as")
	}
	role, name := req.GetRole(), req.GetRoleName()
	switch {
	case role == "" && name != "":
		role = cloudformation.RoleOf(name)
	case role == "":
		role = cloudformation.DefaultRole
	}
	if name == "" {
		name = cloudformation.RoleName(role)
	}
	if cloudformation.RoleOf(name) != role {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			"role_name "+name+" is not a name of the "+role+" template: cluster-vending-"+role+" or cluster-vending-<anything>-"+role)
	}
	region := req.GetRegion()
	if region == "" {
		region = string(awscatalog.USEast1)
	}
	body, err := cloudformation.Template(role)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	out := &cloudv1.AccountOnboarding{Role: role, RoleName: name, StackName: name,
		Region: region, AccountId: req.GetAccountId(), ManagementRoleArn: s.onboarding.ManagementRoleARN,
		TemplateBody: string(body), TemplateFile: cloudformation.FileName(role)}
	if account != nil {
		out.AccountId, out.ExternalId = account.AccountID, account.ExternalID
	}
	if s.onboarding.TemplateURL != "" {
		stack := cloudformation.Stack{Role: role, RoleName: name, Region: region, ManagementRoleARN: s.onboarding.ManagementRoleARN,
			ExternalID: out.GetExternalId()}
		console, err := cloudformation.QuickCreateURL(s.onboarding.TemplateURL, stack)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error())
		}
		out.ConsoleUrl = console
		out.TemplateUrl = cloudformation.TemplateURL(s.onboarding.TemplateURL, role)
	}
	return &cloudv1.GetAccountOnboardingResponse{Onboarding: out}, nil
}
