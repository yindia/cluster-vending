package config_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/config"
)

// The local-credentials component lets the controller get the one
// credentials Secret its setting names, in the namespace the controller is
// deployed to, and write the copies of it in the tenant namespaces; the
// setting it adds loads.
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

// The copies in the tenant namespaces need Secret writes, granted only by
// the component's ClusterRole, bound to the controller.
func TestLocalCredentialsCopierRole(t *testing.T) {
	raw, err := os.ReadFile("local-credentials/cluster_role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	docs := strings.Split(string(raw), "\n---\n")
	role, binding := &rbacv1.ClusterRole{}, &rbacv1.ClusterRoleBinding{}
	if len(docs) != 2 || yaml.UnmarshalStrict([]byte(docs[0]), role) != nil || yaml.UnmarshalStrict([]byte(docs[1]), binding) != nil {
		t.Fatalf("cluster_role.yaml: want a ClusterRole and its binding")
	}
	if len(role.Rules) != 1 || !slices.Equal(role.Rules[0].Resources, []string{"secrets"}) ||
		!slices.Contains(role.Rules[0].Verbs, "create") || !slices.Contains(role.Rules[0].Verbs, "delete") {
		t.Errorf("rules = %+v, want writes on Secrets", role.Rules)
	}
	if binding.RoleRef.Name != role.Name || len(binding.Subjects) != 1 || binding.Subjects[0].Name != "controller-manager" {
		t.Errorf("binding = %+v, want %s bound to the controller", binding, role.Name)
	}
	raw, err = os.ReadFile("local-credentials/kustomization.yaml")
	if err != nil || !strings.Contains(string(raw), "- cluster_role.yaml") {
		t.Errorf("the component does not include cluster_role.yaml (%v)", err)
	}
}
