package component

import (
	"fmt"
	"slices"
	"strings"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// Role is a part of the platform one definition per cloud plays
// (ComponentDefinition spec.role). This file is the one place that knows
// the roles and what each implies; every other package finds the
// definition of a role through the lookups below, never by its name.
// A definition without a role is an ordinary component: sequence and
// requires order it, sizing sizes it, and no Go code names it.
//
// RoleNodeProvisioner (Karpenter) launches the cluster's nodes:
//
//   - it installs first: its sequence is NodeProvisionerSequence whatever
//     its spec says (DefinitionOf), and 0 is no other definition's;
//   - it is the only definition on the system group (${system.*}) and never
//     on the platform pool (${platform.*}), which it launches;
//   - it cannot be disabled: every other platform component waits for it;
//   - the platform pool is applied once it is Ready, and a catalog without
//     one has no Karpenter node groups;
//   - on deletion it is removed last, after the node groups (stage 5).
//
// RoleNamespaceTenancy (Capsule) runs namespace tenancy:
//
//   - it is installed only on clusters with spec.naas (ForNaaS), and what
//     requires it is left out with it;
//   - sub-tenants wait until it is Ready on their cluster;
//   - a new release pin of it carries this build's NaaS bootstrap values
//     (catalog.CapsuleBootstrapValues).
type Role string

// The roles.
const (
	RoleNone             Role = ""
	RoleNodeProvisioner  Role = Role(cloudv1alpha1.ComponentRoleNodeProvisioner)
	RoleNamespaceTenancy Role = Role(cloudv1alpha1.ComponentRoleNamespaceTenancy)
)

// Roles are every role, in a stable order.
var Roles = []Role{RoleNodeProvisioner, RoleNamespaceTenancy}

// NodeProvisionerSequence is the sequence of the NodeProvisioner: it
// installs before every other platform component.
const NodeProvisionerSequence int32 = 0

// legacyRole matches, for a role, the shipped definition of builds before
// spec.role. catalog.Sync leaves a shipped definition the platform team
// edited as it is, so a stored one may lack the role it ships with now. It
// counts only for a cloud on which no definition declares the role, and
// only for a definition that declares none.
var legacyRole = map[Role]func(Definition) bool{
	RoleNodeProvisioner: func(d Definition) bool { return d.Name == "karpenter" },
	RoleNamespaceTenancy: func(d Definition) bool {
		return d.Name == "capsule" || strings.HasSuffix(d.Chart, " capsule")
	},
}

// fallbackName is the name of a role's definition when the catalog cannot
// tell (it cannot be read, or has none): the shipped one's.
var fallbackName = map[Role]string{RoleNodeProvisioner: "karpenter", RoleNamespaceTenancy: "capsule"}

// RoleOf is the role a definition of defs plays: its declared one, or the
// legacy match (legacyRole). provider "" counts every cloud as one.
func RoleOf(d Definition, defs []Definition) Role {
	if d.Role != RoleNone {
		return d.Role
	}
	for _, role := range Roles {
		if legacyRole[role](d) && !declared(defs, role, d.Provider) {
			return role
		}
	}
	return RoleNone
}

// declared reports whether a definition of defs for provider declares role.
func declared(defs []Definition, role Role, provider string) bool {
	return slices.ContainsFunc(defs, func(d Definition) bool { return d.Role == role && d.Provider == provider })
}

// WithRole are the definitions of defs that play role, on provider ("" for
// any), in the order of defs.
func WithRole(defs []Definition, role Role, provider string) []Definition {
	var out []Definition
	for _, d := range defs {
		if (provider == "" || d.Provider == provider) && RoleOf(d, defs) == role {
			out = append(out, d)
		}
	}
	return out
}

// NodeProvisioner is the NodeProvisioner of defs on provider ("" for any);
// false when there is none.
func NodeProvisioner(defs []Definition, provider string) (Definition, bool) {
	return first(WithRole(defs, RoleNodeProvisioner, provider))
}

// NamespaceTenancy is the NamespaceTenancy definition of defs on provider
// ("" for any); false when there is none.
func NamespaceTenancy(defs []Definition, provider string) (Definition, bool) {
	return first(WithRole(defs, RoleNamespaceTenancy, provider))
}

// RoleName is the name of role's definition in defs on provider, or the
// shipped one's when defs has none (a catalog that cannot be read, or one
// whose definition is gone while a cluster still has what it installed).
func RoleName(defs []Definition, role Role, provider string) string {
	if d, ok := first(WithRole(defs, role, provider)); ok {
		return d.Name
	}
	return fallbackName[role]
}

func first(defs []Definition) (Definition, bool) {
	if len(defs) == 0 {
		return Definition{}, false
	}
	return defs[0], true
}

// RoleNameOf is RoleName over the catalog's objects; an object that does
// not decode is left out.
func RoleNameOf(objs []cloudv1alpha1.ComponentDefinition, role Role, provider string) string {
	return RoleName(DefinitionsOf(objs), role, provider)
}

// DefinitionsOf decodes objs, leaving out those that do not decode.
func DefinitionsOf(objs []cloudv1alpha1.ComponentDefinition) []Definition {
	out := make([]Definition, 0, len(objs))
	for i := range objs {
		if d, err := DefinitionOf(&objs[i]); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// checkRoles are the catalog-wide role rules: a role's definition is a
// Platform component, and no two definitions for one cloud declare the
// same role.
func checkRoles(defs []Definition) []string {
	var out []string
	seen := map[string]string{}
	for _, d := range defs {
		if d.Role == RoleNone {
			continue
		}
		if !slices.Contains(Roles, d.Role) {
			out = append(out, fmt.Sprintf("%s: role %q is not one of %v", d.Name, d.Role, Roles))
			continue
		}
		if !d.IsPlatform() {
			out = append(out, fmt.Sprintf("%s: role %s is for a Platform component only", d.Name, d.Role))
		}
		key := string(d.Role) + "/" + d.Provider
		if other, ok := seen[key]; ok {
			out = append(out, fmt.Sprintf("%s and %s both declare role %s on %s: one definition per cloud plays a role", other, d.Name, d.Role, d.Provider))
			continue
		}
		seen[key] = d.Name
	}
	return out
}
