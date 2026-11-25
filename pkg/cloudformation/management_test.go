package cloudformation

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// managementDoc is what the tests read of a management account template.
type managementDoc struct {
	Parameters map[string]struct {
		Default        any    `json:"Default"`
		AllowedPattern string `json:"AllowedPattern"`
		MinValue       int    `json:"MinValue"`
		MaxValue       int    `json:"MaxValue"`
	} `json:"Parameters"`
	Rules     map[string]any `json:"Rules"`
	Outputs   map[string]any `json:"Outputs"`
	Resources map[string]struct {
		Type       string `json:"Type"`
		Properties struct {
			Policies []struct {
				PolicyDocument struct {
					Statement []struct {
						Action   []string `json:"Action"`
						Resource []string `json:"Resource"`
					} `json:"Statement"`
				} `json:"PolicyDocument"`
			} `json:"Policies"`
			ManagedPolicyArns []any `json:"ManagedPolicyArns"`
		} `json:"Properties"`
	} `json:"Resources"`
}

func managementTemplate(t *testing.T, role string) (managementDoc, string) {
	t.Helper()
	body, err := ManagementTemplate(role)
	if err != nil {
		t.Fatal(err)
	}
	var doc managementDoc
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("%s: not YAML: %v", role, err)
	}
	return doc, string(body)
}

// Each management account role is named cluster-vending-<role> by
// default, is trusted through IRSA, a local principal or both (a rule
// requires one), may only assume the tenant roles, and outputs its ARN
// under the name of the setting it goes in.
func TestManagementTemplates(t *testing.T) {
	for _, role := range ManagementRoles() {
		t.Run(role, func(t *testing.T) {
			doc, text := managementTemplate(t, role)
			if got := doc.Parameters[ParameterRoleName].Default; got != RoleName(role) {
				t.Errorf("%s default = %v, want %s", ParameterRoleName, got, RoleName(role))
			}
			for _, p := range []string{ParameterOIDCProviderARN, ParameterLocalPrincipalARN} {
				if param, ok := doc.Parameters[p]; !ok || param.Default != "" || !strings.HasPrefix(param.AllowedPattern, "^$|") {
					t.Errorf("%s = %+v, want optional (default empty)", p, param)
				}
			}
			if got := doc.Parameters[ParameterTenantRolePattern].Default; got != Platform+"-*" {
				t.Errorf("%s default = %v", ParameterTenantRolePattern, got)
			}
			if s := doc.Parameters[ParameterMaxSessionDuration]; s.Default != float64(3600) || s.MinValue != 3600 || s.MaxValue != 43200 {
				t.Errorf("%s = %+v, want 3600 in 3600..43200", ParameterMaxSessionDuration, s)
			}
			if _, ok := doc.Rules["TrustSource"]; !ok {
				t.Error("no rule requiring a trust source")
			}
			r := doc.Resources["Role"]
			if r.Type != "AWS::IAM::Role" || len(doc.Resources) != 1 || len(r.Properties.ManagedPolicyArns) != 0 {
				t.Fatalf("resources = %+v, want the one role", doc.Resources)
			}
			var actions, resources []string
			for _, p := range r.Properties.Policies {
				for _, st := range p.PolicyDocument.Statement {
					actions, resources = append(actions, st.Action...), append(resources, st.Resource...)
				}
			}
			if strings.Join(actions, ",") != "sts:AssumeRole" || len(resources) != 1 {
				t.Errorf("permissions %v on %v, want sts:AssumeRole on the tenant roles only", actions, resources)
			}
			if !strings.Contains(text, "!Sub 'arn:${AWS::Partition}:iam::*:role/${"+ParameterTenantRolePattern+"}'") {
				t.Error("the tenant roles are not arn:<partition>:iam::*:role/<TenantRolePattern>")
			}
			if _, ok := doc.Outputs[RoleARNOutput(role)]; !ok {
				t.Errorf("no output %s: %v", RoleARNOutput(role), doc.Outputs)
			}
			if !strings.Contains(text, `"Action": "sts:AssumeRoleWithWebIdentity"`) || !strings.Contains(text, `"${Issuer}:aud": "sts.amazonaws.com"`) {
				t.Error("no IRSA trust with the sts.amazonaws.com audience")
			}
		})
	}
}

// IRSA trusts exactly the service accounts that run as the role: the
// controller's (a parameter, the Helm chart's name by default) and the
// Crossplane AWS providers' (whose names end in their revision's hash) for
// the management role, the API server's for the server role.
func TestManagementTemplatesTrustTheirServiceAccounts(t *testing.T) {
	doc, management := managementTemplate(t, ManagementRole)
	if got := doc.Parameters[ParameterServiceAccount].Default; got != "cluster-vending-controller" {
		t.Errorf("the controller's service account defaults to %v, want the Helm chart's cluster-vending-controller", got)
	}
	for _, want := range []string{
		`"${Issuer}:sub": "system:serviceaccount:${PlatformNamespace}:${ServiceAccount}"`,
		`"StringLike": {` + "\n" + `                      "${Issuer}:sub": "system:serviceaccount:${CrossplaneNamespace}:provider-aws-*"`,
	} {
		if !strings.Contains(management, want) {
			t.Errorf("the management role does not trust %s", want)
		}
	}
	server, text := managementTemplate(t, ServerRole)
	if got := server.Parameters[ParameterServiceAccount].Default; got != "cluster-vending-server" {
		t.Errorf("the server's service account defaults to %v, want cluster-vending-server", got)
	}
	if strings.Count(text, `"${Issuer}:sub": "system:serviceaccount:${PlatformNamespace}:${ServiceAccount}"`) != 2 ||
		strings.Contains(text, "provider-aws") || strings.Contains(text, "StringLike") {
		t.Error("the server role does not trust only the API server's service account")
	}
	if _, ok := server.Parameters[ParameterCrossplaneNamespace]; ok {
		t.Error("the server role has a Crossplane namespace")
	}
}

func TestUnknownManagementRole(t *testing.T) {
	if _, err := ManagementTemplate("provisioner"); err == nil || !strings.Contains(err.Error(), "management or server") {
		t.Fatalf("err = %v", err)
	}
}
