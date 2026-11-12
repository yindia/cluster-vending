package crd_test

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
)

// The cloudResources rules refuse what they are for.
func TestComponentCloudResourcesRules(t *testing.T) {
	v := newValidator(t, "ComponentDefinition")
	base := `apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ComponentDefinition
metadata:
  name: tools
spec:
  description: d
  namespace: tools
  install:
    helm: {repository: 'https://charts.example.com', chart: tools}
  cloudResources:
    iamRole:
`
	sa := "      serviceAccounts: [{namespace: kube-system, name: tools}]\n"
	policy := "      policy: '{\"Version\":\"2012-10-17\",\"Statement\":[]}'\n"
	managed := func(n int) string {
		out := "      managedPolicyArns:\n"
		for i := range n {
			out += "      - 'arn:aws:iam::${account.id}:policy/p" + string(rune('a'+i)) + "'\n"
		}
		return out
	}
	for name, tt := range map[string]struct {
		role    string
		wantErr bool
	}{
		"policy, IRSA by default": {sa + policy, false},
		"pod identity":            {"      access: PodIdentity\n" + sa + policy, false},
		"aws managed":             {sa + "      managedPolicyArns: ['arn:aws:iam::aws:policy/ReadOnlyAccess']\n", false},
		"policy and 9 managed":    {sa + policy + managed(9), false},
		"policy and 10 managed":   {sa + policy + managed(10), true},
		"nothing to attach":       {sa, true},
		"no service account":      {policy, true},
		"unknown access":          {"      access: Keys\n" + sa + policy, true},
		"another account's ARN placeholder": {sa +
			"      managedPolicyArns: ['arn:aws:iam::${cluster.name}:policy/x']\n", true},
		"trust policy is not a field": {sa + policy + "      trustPolicy: '{}'\n", true},
	} {
		errs := v.validate(t, []byte(base+tt.role))
		if tt.wantErr && len(errs) == 0 {
			t.Errorf("%s: accepted", name)
		}
		if !tt.wantErr && len(errs) > 0 {
			t.Errorf("%s: %v", name, errs.ToAggregate())
		}
	}
}

// The sample definitions use only known placeholders, and their IAM
// policies are policy documents.
func TestComponentDefinitionSamplesPlaceholders(t *testing.T) {
	for i, doc := range sampleDocuments(t, "cloud_v1alpha1_componentdefinition.yaml") {
		def := &cloudv1alpha1.ComponentDefinition{}
		if err := yaml.Unmarshal(doc, def); err != nil {
			t.Fatal(err)
		}
		if err := component.ValidateCloudResources(component.CloudResourcesOf(def.Spec.CloudResources)); err != nil {
			t.Errorf("document %d (%s): %v", i, def.Name, err)
		}
		if def.Spec.PlatformValues == nil {
			continue
		}
		var values any
		if err := json.Unmarshal(def.Spec.PlatformValues.Raw, &values); err != nil {
			t.Fatal(err)
		}
		if err := component.Validate(values); err != nil {
			t.Errorf("document %d (%s) platformValues: %v", i, def.Name, err)
		}
	}
}
