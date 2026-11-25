package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// stackRegion is the region account create's printed command creates the
// stack in: any works, IAM is global.
const stackRegion = "us-east-1"

// AccountCreate onboards the account of a CloudAccount manifest (-f):
//
//  1. GetAccountOnboarding, before anything is made: what the server
//     refuses (no management role configured, a role name that does not
//     match its template) stops it with nothing created.
//  2. TenantService.BootstrapTenant: the account, with its external ID.
//  3. GetAccountOnboarding of that account, and the steps the tenant
//     follows to create the platform's role in AWS (the template the API
//     server serves, its parameters, the create-stack command), then
//     account verify. It makes no AWS call itself.
//
// In table output the steps follow the account on stdout; with -o json or
// yaml stdout is the account alone and the steps go to stderr. Each step
// is the account role's stack and, recommended, the reader role's.
//
// With --management it onboards nothing: it prints how to create the
// management account's roles instead (AccountCreateManagement).
func AccountCreate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if o.Management {
		return nil, AccountCreateManagement(o)
	}
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
	aws := account.GetAws()
	if aws != nil {
		if _, err := onboarding(ctx, c, tenant, "", aws); err != nil {
			return nil, fmt.Errorf("%w\nNothing was created", err)
		}
	}
	res, err := call(ctx, &cloudv1.BootstrapTenantRequest{TenantId: tenant, Account: account}, c.Tenant.BootstrapTenant)
	if err != nil {
		return nil, err
	}
	created := res.(*cloudv1.BootstrapTenantResponse)
	if aws == nil {
		return created, nil
	}
	a := created.GetAccount()
	ob, err := onboarding(ctx, c, a.GetTenantId(), a.GetId(), aws)
	if err != nil {
		return nil, fmt.Errorf("%w\nThe account %s is onboarded: run account create again for the steps to create its role", err, a.GetId())
	}
	steps := roleSteps(o.Server, a, ob)
	if o.Output != OutputTable && o.Output != OutputWide {
		o.progressf("%s", steps)
		return created, nil
	}
	if err := Print(o.Out, created, o.Output); err != nil {
		return nil, err
	}
	_, err = io.WriteString(o.Out, steps)
	return nil, err
}

// onboarding is GetAccountOnboarding of the account's role: of the
// account id once it is onboarded (with its external ID), else only what
// the server can say. A role none of the templates creates asks for the
// default role's, without its name: only the trust is used.
func onboarding(ctx context.Context, c Clients, tenant, id string, aws *cloudv1.AwsAccount) (*cloudv1.AccountOnboarding, error) {
	req := &cloudv1.GetAccountOnboardingRequest{TenantId: tenant, Id: id, AccountId: aws.GetAccountId(),
		Role: cloudformation.RoleOf(aws.GetRole()), RoleName: aws.GetRole()}
	if req.Role == "" {
		req.Role, req.RoleName = cloudformation.DefaultRole, ""
	}
	res, err := call(ctx, req, c.Tenant.GetAccountOnboarding)
	if err != nil {
		return nil, err
	}
	return res.(*cloudv1.GetAccountOnboardingResponse).GetOnboarding(), nil
}

// roleSteps is how the tenant creates the account's role and has its
// access checked: the stack of the template the API server (server)
// serves, or, for a role none of the templates creates, what it must
// trust. Then, recommended, the account's reader role stack (readerStep).
func roleSteps(server string, a *cloudv1.CloudAccount, ob *cloudv1.AccountOnboarding) string {
	aws := a.GetAws()
	url := templateURL(server, ob.GetRole())
	ref := fmt.Sprintf("--id %s --tenant %s", a.GetId(), a.GetTenantId())
	verb, done, action := "create-stack", "CREATE_COMPLETE", "Create"
	if templateOutdated(a) {
		verb, done, action = "update-stack", "UPDATE_COMPLETE", "Update"
	}
	var b strings.Builder
	if cloudformation.RoleOf(aws.GetRole()) == "" {
		fmt.Fprintf(&b, "\nCreate the role %s in account %s yourself, trusting %s", aws.GetRole(), aws.GetAccountId(), ob.GetManagementRoleArn())
		if ob.GetExternalId() != "" {
			fmt.Fprintf(&b, " with external ID %s (sts:ExternalId)", ob.GetExternalId())
		}
		fmt.Fprintf(&b, ", with the permissions of %s.\n", url)
		readerStep(&b, server, aws.GetAccountId(), verb, ob)
		b.WriteString("\nWhen the role is there:")
	} else {
		fmt.Fprintf(&b, "\n%s the IAM role %s in account %s with CloudFormation, with your AWS credentials of that account:\n\n",
			action, ob.GetRoleName(), aws.GetAccountId())
		fmt.Fprintf(&b, "  Template:           %s\n  RoleName:           %s\n  ManagementRoleArn:  %s\n", url, ob.GetRoleName(), ob.GetManagementRoleArn())
		params := []stackParameter{{cloudformation.ParameterManagementRoleARN, ob.GetManagementRoleArn()}}
		if ob.GetServerPrincipalArn() != "" {
			fmt.Fprintf(&b, "  ServerPrincipalArn:  %s\n", ob.GetServerPrincipalArn())
			params = append(params, stackParameter{cloudformation.ParameterServerPrincipalARN, ob.GetServerPrincipalArn()})
		}
		if ob.GetExternalId() != "" {
			fmt.Fprintf(&b, "  ExternalId:         %s\n", ob.GetExternalId())
			params = append(params, stackParameter{cloudformation.ParameterExternalID, ob.GetExternalId()})
		}
		params = append(params, stackParameter{cloudformation.ParameterRoleName, ob.GetRoleName()})
		b.WriteString("\n")
		writeStack(&b, stackCommand{verb: verb, name: ob.GetRoleName(), url: url,
			file: cloudformation.RoleName(ob.GetRole()) + ".yaml", params: params})
		b.WriteString("\n  (any region works: IAM is global, the stack is regional)\n")
		if ob.GetRole() != readerRole {
			readerStep(&b, server, aws.GetAccountId(), verb, ob)
		}
		b.WriteString("\nWhen the stack is " + done + ":")
	}
	fmt.Fprintf(&b, " cluster-vending account verify %s --wait\n", ref)
	fmt.Fprintf(&b, "Then: cluster-vending network create -f <Network manifest with spec.bootstrapId: %s> --tenant %s\n",
		a.GetId(), a.GetTenantId())
	return b.String()
}

// readerRole is the template of the role each cluster's live reads use.
const readerRole = "reader"

// readerStep is the account's reader role stack: optional, recommended.
// Each cluster's live-reader access entry and the live ProviderConfig use
// the reader role (pkg/crossplane/aws, ReaderRoleARN), which trusts the
// management role with the account's external ID like the account's role;
// without it the access entry fails and the cluster has no live reads. The
// API server does not use it, so it trusts no server principal. verb is
// the account role's: an outdated account updates it too.
func readerStep(b *strings.Builder, server, accountID, verb string, ob *cloudv1.AccountOnboarding) {
	name := cloudformation.RoleName(readerRole)
	fmt.Fprintf(b, "\nRecommended: the reader role %s in account %s, which each cluster's live reads use "+
		"(without it the cluster's live-reader access entry fails: \"The specified principalArn is invalid\"):\n\n", name, accountID)
	params := []stackParameter{{cloudformation.ParameterManagementRoleARN, ob.GetManagementRoleArn()}}
	if ob.GetExternalId() != "" {
		params = append(params, stackParameter{cloudformation.ParameterExternalID, ob.GetExternalId()})
	}
	params = append(params, stackParameter{cloudformation.ParameterRoleName, name})
	writeStack(b, stackCommand{verb: verb, name: name, url: templateURL(server, readerRole), file: name + ".yaml", params: params})
	if verb == "update-stack" {
		b.WriteString("\n  (an account without a reader stack yet: the same command with create-stack)\n")
	}
}

// templateURL is where the API server (server) serves the role's template.
func templateURL(server, role string) string {
	return strings.TrimSuffix(server, "/") + cloudformation.TemplatePath(role)
}

// stackParameter is one ParameterKey and its value.
type stackParameter struct{ key, value string }

// stackCommand is a CloudFormation stack of one of the templates the API
// server serves: verb (create-stack or update-stack), the stack's name,
// the template's URL and the file it is saved to, its parameters.
type stackCommand struct {
	verb, name, url, file string
	params                []stackParameter
}

// writeStack writes the commands that fetch the template and create (or
// update) the stack, in stackRegion.
func writeStack(b *strings.Builder, s stackCommand) {
	fmt.Fprintf(b, "  curl -fsSL %s -o %s && \\\n", s.url, s.file)
	fmt.Fprintf(b, "  aws cloudformation %s --stack-name %s \\\n", s.verb, s.name)
	fmt.Fprintf(b, "    --template-body file://%s --capabilities CAPABILITY_NAMED_IAM \\\n", s.file)
	for i, p := range s.params {
		lead := "      "
		if i == 0 {
			lead = "    --parameters "
		}
		fmt.Fprintf(b, "%sParameterKey=%s,ParameterValue=%s \\\n", lead, p.key, p.value)
	}
	fmt.Fprintf(b, "    --region %s\n", stackRegion)
}

// templateOutdated: the account's status says its role's template is older
// than the platform needs (the controller's access check reads its version),
// so its stack is updated, not created.
func templateOutdated(a *cloudv1.CloudAccount) bool {
	for _, c := range a.GetStatus().GetConditions() {
		if c.GetType() == cloudformation.ConditionTemplateCurrent {
			return c.GetStatus() == cloudv1.ConditionStatus_CONDITION_STATUS_FALSE
		}
	}
	return false
}
