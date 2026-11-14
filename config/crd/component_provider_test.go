package crd_test

import "testing"

// A definition is for one of the clouds a cluster runs on.
func TestComponentProviderRule(t *testing.T) {
	v := newValidator(t, "ComponentDefinition")
	doc := func(provider string) []byte {
		return []byte("apiVersion: cloud.cluster-vending.io/v1alpha1\nkind: ComponentDefinition\nmetadata:\n  name: x\nspec:\n" +
			"  provider: " + provider + "\n  description: d\n  namespace: x\n" +
			"  install:\n    helm: {repository: 'https://charts.example.com', chart: x}\n")
	}
	for provider, wantErr := range map[string]bool{"aws": false, "gcp": false, "azure": false, "oracle": true} {
		errs := v.validate(t, doc(provider))
		if wantErr != (len(errs) > 0) {
			t.Errorf("provider %s: errors %v, want refused %v", provider, errs.ToAggregate(), wantErr)
		}
	}
}
