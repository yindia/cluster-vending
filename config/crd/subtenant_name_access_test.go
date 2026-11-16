package crd_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSubTenantAdmissionCanonicalNameAndGroupClaims(t *testing.T) {
	v := newValidator(t, "SubTenant")
	for _, tt := range []struct {
		name  string
		path  []string
		value any
	}{
		{"name too long", []string{"spec", "name"}, strings.Repeat("a", 36)},
		{"invalid name", []string{"spec", "name"}, "Not-DNS"},
		{"empty name", []string{"spec", "name"}, ""},
		{"reserved group", []string{"spec", "access"}, map[string]any{"adminGroups": []any{"system:masters"}}},
		{"wildcard group", []string{"spec", "access"}, map[string]any{"viewerGroups": []any{"team-*"}}},
		{"control group", []string{"spec", "access"}, map[string]any{"viewerGroups": []any{"team\nadmin"}}},
		{"duplicate group", []string{"spec", "access"}, map[string]any{"adminGroups": []any{"team", "team"}}},
		{"overlapping roles", []string{"spec", "access"}, map[string]any{"adminGroups": []any{"team"}, "viewerGroups": []any{"team"}}},
		{"oversized claim", []string{"spec", "access"}, map[string]any{"viewerGroups": []any{strings.Repeat("a", 257)}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := subTenantSample(t)
			if err := unstructured.SetNestedField(obj, tt.value, tt.path...); err != nil {
				t.Fatal(err)
			}
			if errs := v.validate(t, subTenantYAML(t, obj)); len(errs) == 0 {
				t.Fatal("admission accepted invalid name/group contract")
			}
		})
	}
	missing := subTenantSample(t)
	unstructured.RemoveNestedField(missing, "spec", "name")
	if errs := v.validate(t, subTenantYAML(t, missing)); len(errs) == 0 {
		t.Fatal("new native allocation accepted without a name")
	}
	valid := subTenantSample(t)
	if err := unstructured.SetNestedField(valid, map[string]any{"adminGroups": []any{"oidc:payments-admin"}, "viewerGroups": []any{"finance viewers"}}, "spec", "access"); err != nil {
		t.Fatal(err)
	}
	if errs := v.validate(t, subTenantYAML(t, valid)); len(errs) != 0 {
		t.Fatal(errs.ToAggregate())
	}
}

func TestSubTenantAdmissionKeepsLegacyUnnamedIdentityImmutable(t *testing.T) {
	v := newValidator(t, "SubTenant")
	legacy := subTenantSample(t)
	unstructured.RemoveNestedField(legacy, "spec", "name")
	unstructured.RemoveNestedField(legacy, "spec", "deletionProtection")
	before := subTenantYAML(t, legacy)
	if err := unstructured.SetNestedField(legacy, false, "spec", "deletionProtection"); err != nil {
		t.Fatal(err)
	}
	errs, err := v.v.ValidateUpdate(t.Context(), subTenantYAML(t, legacy), before)
	if err != nil || len(errs) != 0 {
		t.Fatalf("legacy protection disable refused: %v, %v", errs, err)
	}
	if err := unstructured.SetNestedField(legacy, "adopted-name", "spec", "name"); err != nil {
		t.Fatal(err)
	}
	errs, err = v.v.ValidateUpdate(t.Context(), subTenantYAML(t, legacy), before)
	if err != nil || len(errs) == 0 {
		t.Fatalf("legacy identity rename accepted: %v, %v", errs, err)
	}
	named := subTenantSample(t)
	before = subTenantYAML(t, named)
	if err := unstructured.SetNestedField(named, "renamed", "spec", "name"); err != nil {
		t.Fatal(err)
	}
	errs, err = v.v.ValidateUpdate(t.Context(), subTenantYAML(t, named), before)
	if err != nil || len(errs) == 0 {
		t.Fatalf("canonical identity rename accepted: %v, %v", errs, err)
	}
}
