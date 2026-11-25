package cloudformation

import (
	"fmt"
	"slices"
)

// The roles of the platform's own (management) AWS account, created once
// before any tenant account (scripts/management.py):
//
//   - ManagementRole: what the controller and the Crossplane AWS providers
//     run as, the aws-management-role-arn setting and the
//     ManagementRoleArn every tenant role trusts.
//   - ServerRole: what the API server runs as, the aws-server-role-arn
//     setting and the ServerPrincipalArn tenant roles trust (it reads the
//     results buckets through them).
//
// Each may only assume the tenant roles, and is trusted through IRSA by
// the service accounts that run as it (ParameterOIDCProviderARN), by a
// local principal (ParameterLocalPrincipalARN), or both.
const (
	ManagementRole = "management"
	ServerRole     = "server"
)

// managementRoles are the management account's roles, in the order they
// are created.
var managementRoles = []string{ManagementRole, ServerRole}

// ManagementRoles returns the management account's roles.
func ManagementRoles() []string { return slices.Clone(managementRoles) }

// Parameters of the management account's templates. RoleName
// (ParameterRoleName) defaults to RoleName(role).
const (
	// ParameterOIDCProviderARN is the IAM OIDC provider of the management
	// EKS cluster: set, the role trusts its service accounts (IRSA).
	ParameterOIDCProviderARN = "OidcProviderArn"
	// ParameterLocalPrincipalARN is an IAM user or role that may assume
	// the role (local development).
	ParameterLocalPrincipalARN = "LocalPrincipalArn"
	// ParameterPlatformNamespace and ParameterCrossplaneNamespace are the
	// namespaces of the trusted service accounts (IRSA);
	// ParameterServiceAccount is the controller's (management) or the API
	// server's (server), the Helm chart's names by default
	// (cluster-vending-controller, cluster-vending-server).
	ParameterPlatformNamespace   = "PlatformNamespace"
	ParameterCrossplaneNamespace = "CrossplaneNamespace"
	ParameterServiceAccount      = "ServiceAccount"
	// ParameterTenantRolePattern is the names of the tenant roles the role
	// may assume (default cluster-vending-*).
	ParameterTenantRolePattern = "TenantRolePattern"
	// ParameterMaxSessionDuration is the role's longest session, seconds.
	ParameterMaxSessionDuration = "MaxSessionDuration"
)

// The outputs holding each management account role's ARN, named after the
// setting it goes in.
const (
	OutputManagementRoleARN = "ManagementRoleArn"
	OutputServerRoleARN     = "ServerRoleArn"
)

// RoleARNOutput is the output of a management account role's ARN.
func RoleARNOutput(role string) string {
	if role == ServerRole {
		return OutputServerRoleARN
	}
	return OutputManagementRoleARN
}

// ManagementTemplate is the CloudFormation template of a management
// account role.
func ManagementTemplate(role string) ([]byte, error) {
	if !slices.Contains(managementRoles, role) {
		return nil, fmt.Errorf("unknown management account role %q: management or server", role)
	}
	return templates.ReadFile("templates/" + fileName(role))
}

// anyTemplate is the template of a tenant or management account role.
func anyTemplate(role string) ([]byte, bool) {
	body, err := Template(role)
	if err != nil {
		body, err = ManagementTemplate(role)
	}
	return body, err == nil
}
