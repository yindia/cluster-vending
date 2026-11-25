package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
	"github.com/yindia/cluster-vending/pkg/config"
)

// AccountCreateManagement is account create --management: how to create
// the roles of the platform's own (management) AWS account, before any
// tenant account is onboarded. It prints, for the management role (what
// the controller and the Crossplane AWS providers run as) and the server
// role (what the API server runs as), the create-stack command of the
// template the API server serves, how to trust IRSA or a local principal,
// and the setting each role's ARN goes in. It makes no API or AWS call:
// the API server serves the templates without a management role
// configured, and the role does not exist yet.
func AccountCreateManagement(o Options) error {
	if o.Input != "" {
		return fmt.Errorf("--management takes no manifest (-f): it prints the management account's roles, not a tenant account's")
	}
	var b strings.Builder
	b.WriteString("Create the platform's roles in its management AWS account, with your AWS credentials of that account, " +
		"before onboarding any tenant account:\n")
	for i, role := range cloudformation.ManagementRoles() {
		managementStep(&b, o.Server, i+1, role)
	}
	b.WriteString("\nThen: cluster-vending account create -f <CloudAccount manifest> for each tenant account; " +
		"its role stacks trust these roles.\n")
	_, err := io.WriteString(o.Out, b.String())
	return err
}

// managementStep is one management account role: its stack, its trust
// (IRSA in production, a local principal otherwise) and its setting.
func managementStep(b *strings.Builder, server string, n int, role string) {
	name := cloudformation.RoleName(role)
	output := cloudformation.RoleARNOutput(role)
	what, setting := "the controller and the Crossplane AWS providers run as", config.FlagAWSManagementRoleARN+
		" of the controller and the API server"
	if role == cloudformation.ServerRole {
		what, setting = "the API server runs as, to read the cluster results buckets (optional: without it the server reads no results)",
			config.FlagAWSServerRoleARN+" of the API server"
	}
	fmt.Fprintf(b, "\n%d. The %s role %s, which %s:\n\n", n, role, name, what)
	writeStack(b, stackCommand{verb: "create-stack", name: name, url: templateURL(server, role), file: name + ".yaml",
		params: []stackParameter{{cloudformation.ParameterOIDCProviderARN, "<OIDC provider ARN>"}}})
	fmt.Fprintf(b, "\n   Production (IRSA): %s is the IAM OIDC provider of the management EKS cluster's issuer\n", cloudformation.ParameterOIDCProviderARN)
	b.WriteString("     (aws eks describe-cluster --name <cluster> --query cluster.identity.oidc.issuer; aws iam list-open-id-connect-providers).\n")
	fmt.Fprintf(b, "   Local (no IRSA): ParameterKey=%s,ParameterValue=<your IAM user or role: aws sts get-caller-identity> instead,\n",
		cloudformation.ParameterLocalPrincipalARN)
	fmt.Fprintf(b, "     and ParameterKey=%s,ParameterValue=43200 for 12-hour sessions. Both may be set.\n", cloudformation.ParameterMaxSessionDuration)
	fmt.Fprintf(b, "   When it is CREATE_COMPLETE, set %s to its %s output:\n", setting, output)
	fmt.Fprintf(b, "     aws cloudformation describe-stacks --stack-name %s --region %s \\\n", name, stackRegion)
	fmt.Fprintf(b, "       --query \"Stacks[0].Outputs[?OutputKey=='%s'].OutputValue\" --output text\n", output)
}
