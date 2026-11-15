package cloudformation

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Every role's template is embedded, parses, trusts the ManagementRoleArn
// parameter and names its role and policies after the RoleName parameter,
// whose default is cluster-vending-<role>.
func TestTemplatesAreEmbedded(t *testing.T) {
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			body, err := Template(role)
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Parameters map[string]struct {
					Default        string `json:"Default"`
					AllowedPattern string `json:"AllowedPattern"`
					NoEcho         bool   `json:"NoEcho"`
				} `json:"Parameters"`
				Resources map[string]struct {
					Type       string         `json:"Type"`
					Properties map[string]any `json:"Properties"`
				} `json:"Resources"`
			}
			if err := yaml.Unmarshal(body, &doc); err != nil {
				t.Fatalf("not YAML: %v", err)
			}
			if _, ok := doc.Parameters[ParameterManagementRoleARN]; !ok || len(doc.Parameters) != 3 {
				t.Errorf("parameters = %v, want %s, %s and %s", doc.Parameters, ParameterManagementRoleARN, ParameterRoleName, ParameterExternalID)
			}
			// The external ID is optional (older accounts have none) and never
			// shown back by CloudFormation.
			if ext, ok := doc.Parameters[ParameterExternalID]; !ok || !ext.NoEcho || ext.Default != "" ||
				!strings.HasPrefix(ext.AllowedPattern, "^$|") {
				t.Errorf("%s = %+v, want NoEcho, default empty, empty allowed", ParameterExternalID, ext)
			}
			name, ok := doc.Parameters[ParameterRoleName]
			if !ok || name.Default != RoleName(role) || !strings.HasSuffix(name.AllowedPattern, role+"$") {
				t.Errorf("%s = %+v, want default %s and a pattern ending in %s", ParameterRoleName, name, RoleName(role), role)
			}
			r, ok := doc.Resources["Role"]
			if !ok || r.Type != "AWS::IAM::Role" {
				t.Fatalf("no AWS::IAM::Role named Role: %v", doc.Resources)
			}
			// The YAML has CloudFormation's short forms (!Ref, !Sub), which a
			// YAML decoder drops: read those lines as written.
			text := string(body)
			// With an external ID the trust requires it; without one, not.
			if !strings.Contains(text, "sts:ExternalId: !Ref '"+ParameterExternalID+"'") || !strings.Contains(text, "- "+ConditionHasExternalID) {
				t.Errorf("the trust does not require sts:ExternalId under %s", ConditionHasExternalID)
			}
			if !strings.Contains(text, "RoleName: !Ref '"+ParameterRoleName+"'") {
				t.Errorf("the role is not named !Ref %s", ParameterRoleName)
			}
			policies := 0
			for _, line := range strings.Split(text, "\n") {
				name, ok := strings.CutPrefix(strings.TrimSpace(line), "ManagedPolicyName: ")
				if !ok {
					continue
				}
				policies++
				if !strings.HasPrefix(name, "!Sub '${"+ParameterRoleName+"}-") {
					t.Errorf("policy %s, want !Sub '${%s}-<part>'", name, ParameterRoleName)
				}
			}
			if policies == 0 {
				t.Error("no managed policy")
			}
		})
	}
}

func TestUnknownRole(t *testing.T) {
	if _, err := Template("admin"); err == nil || !strings.Contains(err.Error(), "reader, updater or provisioner") {
		t.Fatalf("err = %v", err)
	}
}

// A role name says its template by its last word: cluster-vending-<role>,
// or cluster-vending-<anything>-<role> for a stack of its own.
func TestRoleOfName(t *testing.T) {
	tests := []struct{ name, want string }{
		{"cluster-vending-provisioner", "provisioner"},
		{"cluster-vending-reader", "reader"},
		{"cluster-vending-demo-0115-provisioner", "provisioner"},
		{"cluster-vending-team-a-updater", "updater"},
		{"cluster-vending-provisioner-old", ""},
		{"acme-provisioner", ""},
		{"acme-admin", ""},
		{"cluster-vending-" + strings.Repeat("x", 50) + "-reader", ""}, // over IAM's 64 characters
	}
	for _, tt := range tests {
		if got := RoleOf(tt.name); got != tt.want {
			t.Errorf("RoleOf(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
	if DefaultRole != "provisioner" {
		t.Errorf("DefaultRole = %q", DefaultRole)
	}
}
