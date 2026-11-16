package crd_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func subTenantSample(t *testing.T) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := yaml.Unmarshal(sampleDocuments(t, "cloud_v1alpha1_subtenant.yaml")[0], &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func subTenantYAML(t *testing.T, obj map[string]any) []byte {
	t.Helper()
	raw, err := yaml.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSubTenantAdmission(t *testing.T) {
	v := newValidator(t, "SubTenant")
	if errs := v.validate(t, subTenantYAML(t, subTenantSample(t))); len(errs) != 0 {
		t.Fatal(errs.ToAggregate())
	}
	for _, tt := range []struct {
		name  string
		path  []string
		value any
	}{
		{"unknown privilege", []string{"spec", "privileged"}, true},
		{"unapproved profile", []string{"spec", "policyProfile"}, "unrestricted"},
		{"invalid namespace key", []string{"spec", "namespaceKeys"}, []any{"Uppercase"}},
		{"insufficient namespace budget", []string{"spec", "quota", "namespaceCount"}, int64(0)},
		{"unsupported quota dimension", []string{"spec", "quota", "hard"}, map[string]any{"unbounded": "1"}},
		{"negative quantity", []string{"spec", "quota", "hard"}, map[string]any{"requests.cpu": "-1"}},
		{"malformed quantity", []string{"spec", "quota", "hard"}, map[string]any{"requests.memory": "lots"}},
		{"fractional object count", []string{"spec", "quota", "hard"}, map[string]any{"pods": "1.5"}},
		{"unapproved role", []string{"spec", "access", "bindings"}, []any{map[string]any{"identityRef": "team", "roleProfile": "cluster-admin"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := subTenantSample(t)
			if err := unstructured.SetNestedField(obj, tt.value, tt.path...); err != nil {
				t.Fatal(err)
			}
			if errs := v.validate(t, subTenantYAML(t, obj)); len(errs) == 0 {
				t.Fatal("admission accepted invalid SubTenant")
			}
		})
	}
}

func TestSubTenantAdmissionProtectsParentAndNamespaceHistory(t *testing.T) {
	v := newValidator(t, "SubTenant")
	before := subTenantYAML(t, subTenantSample(t))
	for _, tt := range []struct {
		name  string
		path  []string
		value any
	}{
		{"tenant incarnation", []string{"spec", "tenantRef", "uid"}, "replacement-tenant"},
		{"cluster incarnation", []string{"spec", "clusterRef", "uid"}, "replacement-cluster"},
		{"namespace rename", []string{"spec", "namespaceKeys"}, []any{"renamed"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := subTenantSample(t)
			if err := unstructured.SetNestedField(obj, tt.value, tt.path...); err != nil {
				t.Fatal(err)
			}
			errs, err := v.v.ValidateUpdate(t.Context(), subTenantYAML(t, obj), before)
			if err != nil || len(errs) == 0 {
				t.Fatalf("transition errors = %v, %v; expected refusal", errs, err)
			}
		})
	}
	after := subTenantSample(t)
	if err := unstructured.SetNestedSlice(after, []any{"app", "worker"}, "spec", "namespaceKeys"); err != nil {
		t.Fatal(err)
	}
	errs, err := v.v.ValidateUpdate(t.Context(), subTenantYAML(t, after), before)
	if err != nil || len(errs) != 0 {
		t.Fatalf("append-only allocation refused: %v, %v", errs, err)
	}
}
