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
			wantParameters := 4
			if role == DefaultRole {
				wantParameters = 5 // and ParameterCreateSpotServiceLinkedRole
			}
			if _, ok := doc.Parameters[ParameterManagementRoleARN]; !ok || len(doc.Parameters) != wantParameters {
				t.Errorf("parameters = %v, want %s, %s, %s and %s (and %s on the provisioner)", doc.Parameters,
					ParameterManagementRoleARN, ParameterRoleName, ParameterExternalID, ParameterServerPrincipalARN,
					ParameterCreateSpotServiceLinkedRole)
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

// Karpenter's Spot nodes need the account's EC2 Spot service-linked role.
// CloudFormation fails to create a resource that exists, and an account that
// ever used Spot has the role: the provisioner template creates it only when
// asked (a parameter, off by default so that no stack update of an existing
// account can fail on it), keeps it when the stack goes (it is the
// account's, not the stack's), and the other roles do not have it.
func TestProvisionerTemplateCreatesTheSpotServiceLinkedRoleOnlyOnRequest(t *testing.T) {
	for _, role := range roles {
		body, err := Template(role)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Parameters map[string]struct {
				Default       string   `json:"Default"`
				AllowedValues []string `json:"AllowedValues"`
			} `json:"Parameters"`
			Conditions map[string]any `json:"Conditions"`
			Resources  map[string]struct {
				Type           string         `json:"Type"`
				Condition      string         `json:"Condition"`
				DeletionPolicy string         `json:"DeletionPolicy"`
				Properties     map[string]any `json:"Properties"`
			} `json:"Resources"`
		}
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("%s: not YAML: %v", role, err)
		}
		var found []string
		for name, r := range doc.Resources {
			if r.Type == "AWS::IAM::ServiceLinkedRole" {
				found = append(found, name)
				if r.Properties["AWSServiceName"] != "spot.amazonaws.com" || r.Condition != ConditionCreateSpotServiceLinkedRole ||
					r.DeletionPolicy != "Retain" {
					t.Errorf("%s: %s = %+v, want spot.amazonaws.com, condition %s, retained", role, name, r, ConditionCreateSpotServiceLinkedRole)
				}
			}
		}
		if role != DefaultRole {
			if len(found) != 0 || doc.Parameters[ParameterCreateSpotServiceLinkedRole].Default != "" {
				t.Errorf("%s has the Spot service-linked role: %v", role, found)
			}
			continue
		}
		param := doc.Parameters[ParameterCreateSpotServiceLinkedRole]
		if len(found) != 1 || param.Default != "false" || strings.Join(param.AllowedValues, ",") != "true,false" {
			t.Errorf("provisioner: resources %v, parameter %+v, want one role, default false, allowed true and false", found, param)
		}
		if _, ok := doc.Conditions[ConditionCreateSpotServiceLinkedRole]; !ok {
			t.Errorf("provisioner: no condition %s", ConditionCreateSpotServiceLinkedRole)
		}
		if !strings.Contains(string(body), "!Equals\n    - !Ref 'CreateSpotServiceLinkedRole'\n    - 'true'") {
			t.Error("the condition is not CreateSpotServiceLinkedRole == 'true'")
		}
	}
}

// The API server reads the results buckets through the tenant role, so the
// role trusts the server's principal next to the management role. The
// parameter is optional (an account whose server has no AWS access keeps
// the one principal), and the external ID, when there is one, is required
// of both.
func TestTemplatesTrustTheServerPrincipalWhenGiven(t *testing.T) {
	for _, role := range roles {
		body, err := Template(role)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Parameters map[string]struct {
				Default        string `json:"Default"`
				AllowedPattern string `json:"AllowedPattern"`
			} `json:"Parameters"`
			Conditions map[string]any `json:"Conditions"`
		}
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		param, ok := doc.Parameters[ParameterServerPrincipalARN]
		if !ok || param.Default != "" || !strings.HasPrefix(param.AllowedPattern, "^$|^arn:aws") {
			t.Errorf("%s: %s = %+v, want default empty, empty or a role ARN allowed", role, ParameterServerPrincipalARN, param)
		}
		if _, ok := doc.Conditions[ConditionHasServerPrincipal]; !ok {
			t.Errorf("%s: no condition %s", role, ConditionHasServerPrincipal)
		}
		text := string(body)
		// Four trusts: with or without a server principal, with or without
		// an external ID. The server principal is listed in the two that have
		// it, and once in the condition on it.
		if n := strings.Count(text, "- !Ref '"+ParameterServerPrincipalARN+"'"); n != 3 {
			t.Errorf("%s: the server principal is referenced %d times, want 3 (the condition and two trusts)", role, n)
		}
		if n := strings.Count(text, "- "+ConditionHasServerPrincipal); n < 2 {
			t.Errorf("%s: the trust is not chosen by %s", role, ConditionHasServerPrincipal)
		}
	}
}

// What the platform does with the results buckets is in the provisioner
// role only, on cv-* buckets and nothing else: creating, configuring and
// deleting the bucket, and reading, writing and deleting its objects.
func TestProvisionerRoleHasTheResultsBucketPermissions(t *testing.T) {
	type statement struct {
		Sid      string   `json:"Sid"`
		Effect   string   `json:"Effect"`
		Action   []string `json:"Action"`
		Resource []string `json:"Resource"`
	}
	for _, role := range roles {
		body, err := Template(role)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Resources map[string]struct {
				Type       string `json:"Type"`
				Properties struct {
					PolicyDocument struct {
						Statement []statement `json:"Statement"`
					} `json:"PolicyDocument"`
				} `json:"Properties"`
			} `json:"Resources"`
		}
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		var s3Statements []statement
		for _, r := range doc.Resources {
			for _, st := range r.Properties.PolicyDocument.Statement {
				for _, a := range st.Action {
					if strings.HasPrefix(a, "s3:") {
						s3Statements = append(s3Statements, st)
						break
					}
				}
			}
		}
		if role != DefaultRole {
			if len(s3Statements) != 0 {
				t.Errorf("%s has S3 permissions: %+v", role, s3Statements)
			}
			continue
		}
		granted := map[string]string{} // action -> resource
		for _, st := range s3Statements {
			if st.Effect != "Allow" {
				t.Errorf("S3 statement %+v is not an Allow", st)
			}
			for _, res := range st.Resource {
				if res != "arn:aws:s3:::cv-*" && res != "arn:aws:s3:::cv-*/*" {
					t.Errorf("S3 statement %s on %s: only cv-* buckets and their objects", st.Sid, res)
				}
				for _, a := range st.Action {
					granted[a] = res
				}
			}
		}
		for action, resource := range map[string]string{
			"s3:CreateBucket": "arn:aws:s3:::cv-*", "s3:DeleteBucket": "arn:aws:s3:::cv-*", "s3:ListBucket": "arn:aws:s3:::cv-*",
			"s3:PutBucketPublicAccessBlock": "arn:aws:s3:::cv-*", "s3:PutBucketOwnershipControls": "arn:aws:s3:::cv-*",
			"s3:PutEncryptionConfiguration": "arn:aws:s3:::cv-*", "s3:PutBucketPolicy": "arn:aws:s3:::cv-*",
			"s3:PutLifecycleConfiguration": "arn:aws:s3:::cv-*", "s3:PutBucketTagging": "arn:aws:s3:::cv-*",
			"s3:GetObject": "arn:aws:s3:::cv-*/*", "s3:PutObject": "arn:aws:s3:::cv-*/*", "s3:DeleteObject": "arn:aws:s3:::cv-*/*",
		} {
			if granted[action] != resource {
				t.Errorf("%s is granted on %q, want %q", action, granted[action], resource)
			}
		}
		for action := range granted {
			if action == "s3:*" || strings.HasSuffix(action, ":*") {
				t.Errorf("wildcard S3 action %s", action)
			}
		}
	}
}
