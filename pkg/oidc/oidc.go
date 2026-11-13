// Package oidc resolves a cluster's OIDC identity provider
// (spec.access.oidc) to the settings the Kubernetes API is given: as they
// are for a generic provider, filled in for Microsoft Entra ID. It is pure:
// the renderer and the CLI's kubeconfig both read the same result.
package oidc

import (
	"maps"
	"slices"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// What the platform sets for Microsoft Entra ID.
const (
	// EntraName is the provider's name when the spec gives none.
	EntraName = "entra"
	// EntraUsernameClaim is the user's object ID: immutable, and the same
	// for every application of the tenant. upn, email and
	// preferred_username can change and can be reused.
	EntraUsernameClaim = "oid"
	// EntraGroupsClaim lists the object IDs of the user's groups.
	EntraGroupsClaim = "groups"
	// EntraPrefix is put in front of user and group names: RBAC subjects
	// are "entra:<object id>".
	EntraPrefix = "entra:"

	entraIssuerPrefix = "https://login.microsoftonline.com/"
	entraIssuerSuffix = "/v2.0"
)

// Provider is an OIDC identity provider as the Kubernetes API takes it.
type Provider struct {
	Name, IssuerURL, ClientID                                string
	UsernameClaim, UsernamePrefix, GroupsClaim, GroupsPrefix string
	RequiredClaims                                           map[string]string
	// Entra reports that the provider is Microsoft Entra ID (the preset).
	Entra bool
}

// EntraIssuer is the issuer of a tenant's v2.0 tokens: the "iss" claim of
// an ID token requested from the v2.0 endpoint.
func EntraIssuer(tenantID string) string { return entraIssuerPrefix + tenantID + entraIssuerSuffix }

// Resolve is the provider spec asks for; nil for none. The spec is taken
// as valid (the contract checks it).
func Resolve(spec *cloudv1.OidcIdentityProvider) *Provider {
	if spec == nil {
		return nil
	}
	p := &Provider{Name: spec.GetName(), RequiredClaims: maps.Clone(spec.GetRequiredClaims())}
	if e := spec.GetEntra(); e != nil {
		if p.Name == "" {
			p.Name = EntraName
		}
		p.Entra = true
		p.IssuerURL, p.ClientID = EntraIssuer(e.GetTenantId()), e.GetClientId()
		p.UsernameClaim, p.UsernamePrefix = EntraUsernameClaim, EntraPrefix
		p.GroupsClaim, p.GroupsPrefix = EntraGroupsClaim, EntraPrefix
		return p
	}
	p.IssuerURL, p.ClientID = spec.GetIssuerUrl(), spec.GetClientId()
	p.UsernameClaim, p.UsernamePrefix = spec.GetUsernameClaim(), spec.GetUsernamePrefix()
	p.GroupsClaim, p.GroupsPrefix = spec.GetGroupsClaim(), spec.GetGroupsPrefix()
	return p
}

// Scopes are the scopes a client asks the provider for, beyond "openid",
// so that its ID token carries the claims the cluster reads:
//
//   - Entra: "profile" (the "oid" claim needs it) and "offline_access" (a
//     refresh token, so kubectl does not open the browser every hour);
//   - a generic provider: "email" or "profile" when the user name claim is
//     one of theirs, and "groups" when the groups claim is "groups" (the
//     scope most providers put it behind; Entra has no such scope).
func (p Provider) Scopes() []string {
	if p.Entra {
		return []string{"profile", "offline_access"}
	}
	var out []string
	switch p.UsernameClaim {
	case "email":
		out = append(out, "email")
	case "name", "preferred_username", "nickname":
		out = append(out, "profile")
	}
	if p.GroupsClaim == "groups" {
		out = append(out, "groups")
	}
	slices.Sort(out)
	return out
}
