package config_test

import (
	"os"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/config"
)

// The local-credentials component lets the controller get the one
// credentials Secret its setting names, in the namespace the controller
// is deployed to, and nothing else; the setting it adds loads.
func TestLocalCredentialsComponent(t *testing.T) {
	raw, err := os.ReadFile("local-credentials/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.Role{}
	if err := yaml.UnmarshalStrict(raw, role); err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("rules = %+v, want one", role.Rules)
	}
	rule := role.Rules[0]
	if !slices.Equal(rule.APIGroups, []string{""}) || !slices.Equal(rule.Resources, []string{"secrets"}) ||
		!slices.Equal(rule.Verbs, []string{"get"}) || len(rule.ResourceNames) != 1 {
		t.Errorf("rule = %+v, want get on one named Secret", rule)
	}

	raw, err = os.ReadFile("local-credentials/manager_patch.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var ops []struct {
		Value struct{ Name, Value string } `json:"value"`
	}
	if err := yaml.Unmarshal(raw, &ops); err != nil || len(ops) != 1 || ops[0].Value.Name != config.EnvProviderCredentialsSecret {
		t.Fatalf("patch = %+v (%v), want the %s variable", ops, err, config.EnvProviderCredentialsSecret)
	}
	cfg, err := config.LoadController(nil, lookup(map[string]string{
		config.EnvAWSManagementRoleARN:      "arn:aws:iam::111122223333:role/crossplane-provider-aws",
		config.EnvProviderCredentialsSecret: ops[0].Value.Value,
	}), nil)
	if err != nil {
		t.Fatalf("the setting does not load: %v", err)
	}
	ref := cfg.AWS.ProviderCredentials
	if ref == nil || ref.Name != rule.ResourceNames[0] || ref.Namespace != "cluster-vending-system" {
		t.Errorf("setting %+v, role on Secret %s: they must name the same Secret, in the controller's namespace",
			ref, rule.ResourceNames[0])
	}
}
