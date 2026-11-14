package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// AccountTemplate prints how to create the platform's IAM role in a
// tenant AWS account with CloudFormation (TenantService.GetAccountOnboarding):
// it writes the role's template to --out (or a new temporary file) and
// prints the AWS CLI command, the console link when the server has one,
// and what to run once the stack exists. With --id the account's role and
// account ID are the onboarded account's.
func AccountTemplate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	role, accountID := o.Role, o.AccountID
	if o.ID != "" {
		res, err := call(ctx, &cloudv1.GetCloudAccountRequest{TenantId: o.Tenant, Id: o.ID}, c.Tenant.GetCloudAccount)
		if err != nil {
			return nil, err
		}
		aws := res.(*cloudv1.GetCloudAccountResponse).GetAccount().GetAws()
		if accountID == "" {
			accountID = aws.GetAccountId()
		}
		if role == "" {
			if role = cloudformation.RoleOf(aws.GetRole()); role == "" {
				return nil, fmt.Errorf("account %s uses the role %s, which no template creates; pass --role reader, updater or provisioner to print one anyway",
					o.ID, aws.GetRole())
			}
		}
	}
	if role == "" {
		role = cloudformation.DefaultRole
	}
	res, err := call(ctx, &cloudv1.GetAccountOnboardingRequest{TenantId: o.Tenant, Role: role, Region: o.StackRegion,
		AccountId: accountID}, c.Tenant.GetAccountOnboarding)
	if err != nil {
		return nil, err
	}
	onboarding := res.(*cloudv1.GetAccountOnboardingResponse).GetOnboarding()
	path, err := writeTemplate(o.TemplateOut, onboarding)
	if err != nil {
		return nil, err
	}
	if !isTable(o.Output) {
		return res, nil
	}
	return nil, printOnboarding(o.Out, o, onboarding, path)
}

// accountNext is what account create prints after the account: how to
// create the role in the AWS account, if it does not exist yet, and how
// to have it checked.
func accountNext(w io.Writer, a *cloudv1.CloudAccount) error {
	aws := a.GetAws()
	if aws == nil {
		return nil
	}
	ref := fmt.Sprintf("--id %s --tenant %s", a.GetId(), a.GetTenantId())
	var b strings.Builder
	b.WriteString("\nNext, if the role does not exist in the account yet:\n")
	if cloudformation.RoleOf(aws.GetRole()) != "" {
		fmt.Fprintf(&b, "  cluster-vending account template %s   (the stack that creates %s)\n", ref, aws.GetRole())
	} else {
		fmt.Fprintf(&b, "  %s is not a role the platform's templates create: create it yourself, or create one with\n"+
			"  cluster-vending account template --role provisioner --account-id %s --tenant %s and onboard the account with its name\n",
			aws.GetRole(), aws.GetAccountId(), a.GetTenantId())
	}
	fmt.Fprintf(&b, "Then: cluster-vending account verify %s\n", ref)
	_, err := io.WriteString(w, b.String())
	return err
}

// writeTemplate writes the template to path, or to a new file in the
// temporary directory when path is empty, and returns where.
func writeTemplate(path string, ob *cloudv1.AccountOnboarding) (string, error) {
	if path == "" {
		f, err := os.CreateTemp("", strings.TrimSuffix(ob.GetTemplateFile(), ".template.yaml")+"-*.template.yaml")
		if err != nil {
			return "", fmt.Errorf("write the template: %w", err)
		}
		path = f.Name()
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("write the template: %w", err)
		}
	}
	if err := os.WriteFile(path, []byte(ob.GetTemplateBody()), 0o644); err != nil { //nolint:gosec // a CloudFormation template, no secret
		return "", fmt.Errorf("write the template: %w", err)
	}
	return path, nil
}

// printOnboarding is account template's output: what the stack creates,
// where the template is, and the commands to run.
func printOnboarding(w io.Writer, o Options, ob *cloudv1.AccountOnboarding, path string) error {
	kv := [][2]string{
		{"Role", fmt.Sprintf("%s (IAM role %s)", ob.GetRole(), ob.GetRoleName())},
		{"Trusts", ob.GetManagementRoleArn()},
		{"Template", path},
	}
	if url := cloudformation.DownloadURL(o.Server, ob.GetRole()); url != "" {
		kv = append(kv, [2]string{"Download", url})
	}
	if ob.GetConsoleUrl() != "" {
		kv = append(kv, [2]string{"Console", ob.GetConsoleUrl()})
	}
	if err := fields(w, kv); err != nil {
		return err
	}
	who := "of the tenant account"
	if ob.GetAccountId() != "" {
		who = "of account " + ob.GetAccountId()
	}
	stack := cloudformation.Stack{Role: ob.GetRole(), Region: ob.GetRegion(), ManagementRoleARN: ob.GetManagementRoleArn()}
	var b strings.Builder
	fmt.Fprintf(&b, "\nCreate the stack with credentials %s", who)
	if ob.GetConsoleUrl() != "" {
		b.WriteString(" (or open Console, signed in to that account)")
	}
	fmt.Fprintf(&b, ":\n  %s\n", cloudformation.CreateStackCommand(stack, path))
	if ob.GetConsoleUrl() == "" {
		fmt.Fprintf(&b, "\nFor a console link, the platform team uploads pkg/cloudformation/templates to an S3 bucket and sets\n"+
			"cloudformation-template-url on the API server (CloudFormation reads templates only from S3).\n")
	}
	b.WriteString("\nOnce the stack exists:\n")
	id := o.ID
	if id == "" {
		fmt.Fprintf(&b, "  cluster-vending account create -f <CloudAccount manifest with spec.aws.role: %s> --tenant %s\n",
			ob.GetRoleName(), o.Tenant)
		id = "<id>"
	}
	fmt.Fprintf(&b, "  cluster-vending account verify --id %s --tenant %s\n", id, o.Tenant)
	_, err := io.WriteString(w, b.String())
	return err
}
