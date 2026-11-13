package auth

import (
	"context"
	"slices"

	"github.com/yindia/cluster-vending/pkg/fault"
)

// Identity is the caller of a request.
type Identity struct {
	// Anonymous: no token was sent, in a mode that lets such requests
	// through (none, optional). An anonymous caller may do everything, as
	// before authentication existed.
	Anonymous bool
	// Subject is the token's "sub".
	Subject string
	// Username is the issuer's username claim, for people to read.
	Username string
	// Issuer is the name of the Issuer that vouched for the caller.
	Issuer string
	// Roles are the values of the issuer's roles claim.
	Roles []string
	// Grants are what the rules give the caller.
	Grants []Grant
}

// Anonymous is the caller of a request without a token.
func Anonymous() *Identity { return &Identity{Anonymous: true} }

type identityKey struct{}

// WithIdentity returns ctx carrying the caller.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext is the caller ctx carries; anonymous when it carries none (a
// handler called without the interceptor).
func FromContext(ctx context.Context) *Identity {
	if id, ok := ctx.Value(identityKey{}).(*Identity); ok && id != nil {
		return id
	}
	return Anonymous()
}

// IsPlatformAdmin reports whether a grant makes the caller a platform
// admin.
func (i *Identity) IsPlatformAdmin() bool {
	return slices.ContainsFunc(i.Grants, func(g Grant) bool { return g.Role == RolePlatformAdmin })
}

// Authorize says whether the caller may make a call that needs access, on
// tenant for a tenant call: nil, or a PermissionDenied fault that says
// what is missing.
func (i *Identity) Authorize(access Access, tenant string) error {
	if i.Anonymous || i.IsPlatformAdmin() {
		return nil
	}
	switch access.Scope {
	case ScopeAuthenticated:
		return nil
	case ScopePlatform:
		return fault.New(fault.PermissionDenied, "this call needs the PlatformAdmin role")
	case ScopeTenant:
		return i.authorizeTenant(access.Write, tenant)
	default:
		return fault.New(fault.PermissionDenied, "this call is not open to callers")
	}
}

// authorizeTenant checks the caller's tenant grants.
func (i *Identity) authorizeTenant(write bool, tenant string) error {
	if tenant == "" {
		return fault.New(fault.PermissionDenied, "the request names no tenant")
	}
	readOnly := false
	for _, g := range i.Grants {
		if !slices.Contains(g.Tenants, tenant) && !slices.Contains(g.Tenants, AllTenants) {
			continue
		}
		switch {
		case g.Role == RoleTenantAdmin, g.Role == RoleTenantViewer && !write:
			return nil
		case g.Role == RoleTenantViewer:
			readOnly = true
		}
	}
	if readOnly {
		return fault.Newf(fault.PermissionDenied, "your access to tenant %s is read-only", tenant)
	}
	return fault.Newf(fault.PermissionDenied, "you have no access to tenant %s", tenant)
}

// grantsFor are the grants the rules give a caller of issuer with roles
// and, when the issuer has a tenant claim, the tenant in it.
func grantsFor(rules []Rule, issuer Issuer, roles []string, claimTenant string) []Grant {
	var out []Grant
	for _, r := range rules {
		if r.Match.Issuer != issuer.Name || (r.Match.Role != "" && !slices.Contains(roles, r.Match.Role)) {
			continue
		}
		g := Grant{Role: r.Grant.Role, Tenants: slices.Clone(r.Grant.Tenants)}
		if g.Role != RolePlatformAdmin && len(g.Tenants) == 0 {
			// The tenant comes from the token. Without a usable value the
			// rule grants nothing.
			if !tenantPattern.MatchString(claimTenant) {
				continue
			}
			g.Tenants = []string{claimTenant}
		}
		out = append(out, g)
	}
	return out
}
