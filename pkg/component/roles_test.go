package component_test

import (
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/component"
)

func TestRolesAreFoundByRoleThenByTheShippedName(t *testing.T) {
	declared := component.Definition{Name: "nodes", Provider: "aws", Role: component.RoleNodeProvisioner, Management: component.Platform}
	legacy := component.Definition{Name: "karpenter", Provider: "aws", Management: component.Platform}
	gcp := component.Definition{Name: "karpenter", Provider: "gcp", Management: component.Platform}
	tests := []struct {
		name string
		defs []component.Definition
		cloud,
		want string
	}{
		{"a declared role wins over the shipped name", []component.Definition{legacy, declared}, "aws", "nodes"},
		{"without a declared role the shipped name stands for it", []component.Definition{legacy}, "aws", "karpenter"},
		{"a role declared on another cloud does not count", []component.Definition{declared, gcp}, "gcp", "karpenter"},
		{"without a catalog the shipped name", nil, "aws", "karpenter"},
	}
	for _, tt := range tests {
		if got := component.RoleName(tt.defs, component.RoleNodeProvisioner, tt.cloud); got != tt.want {
			t.Errorf("%s: RoleName = %s, want %s", tt.name, got, tt.want)
		}
	}
	if component.RoleOf(legacy, []component.Definition{legacy, declared}) != component.RoleNone {
		t.Error("karpenter is ordinary once another definition declares the role")
	}
}

func TestCheckCatalogHoldsOneDefinitionPerRolePerCloud(t *testing.T) {
	def := func(name, provider string, role component.Role, m component.Management) component.Definition {
		return component.Definition{Name: name, Provider: provider, Role: role, Management: m}
	}
	tests := []struct {
		name string
		defs []component.Definition
		want string // "" is no role problem
	}{
		{"one per cloud", []component.Definition{def("a", "aws", component.RoleNodeProvisioner, component.Platform),
			def("b", "gcp", component.RoleNodeProvisioner, component.Platform)}, ""},
		{"two on one cloud", []component.Definition{def("a", "aws", component.RoleNodeProvisioner, component.Platform),
			def("b", "aws", component.RoleNodeProvisioner, component.Platform)}, "a and b both declare role NodeProvisioner on aws"},
		{"a tenant component", []component.Definition{def("t", "aws", component.RoleNodeProvisioner, component.Tenant)}, "t: role NodeProvisioner is for a Platform component only"},
		{"an unknown role", []component.Definition{def("x", "aws", "Gateway", component.Platform)}, `x: role "Gateway" is not one of`},
		{"the removed NamespaceTenancy role", []component.Definition{def("capsule", "aws", "NamespaceTenancy", component.Platform)}, `capsule: role "NamespaceTenancy" is not one of`},
	}
	for _, tt := range tests {
		got := strings.Join(component.CheckCatalog(tt.defs), "\n")
		if tt.want == "" && strings.Contains(got, "role") || tt.want != "" && !strings.Contains(got, tt.want) {
			t.Errorf("%s: problems %q, want %q", tt.name, got, tt.want)
		}
	}
}
