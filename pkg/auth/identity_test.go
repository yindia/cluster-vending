package auth_test

import (
	"context"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/fault"
)

func TestAuthorize(t *testing.T) {
	read := auth.Access{Scope: auth.ScopeTenant}
	write := auth.Access{Scope: auth.ScopeTenant, Write: true}
	platform := auth.Access{Scope: auth.ScopePlatform, Write: true}
	anyone := auth.Access{Scope: auth.ScopeAuthenticated}
	grants := func(g ...auth.Grant) *auth.Identity { return &auth.Identity{Subject: "u", Grants: g} }
	admin := auth.Grant{Role: auth.RoleTenantAdmin, Tenants: []string{"acme"}}
	viewer := auth.Grant{Role: auth.RoleTenantViewer, Tenants: []string{"acme"}}
	tests := []struct {
		name   string
		id     *auth.Identity
		access auth.Access
		tenant string
		deny   string // "" = allowed
	}{
		{"anonymous may do everything", auth.Anonymous(), platform, "acme", ""},
		{"platform admin: platform call", grants(auth.Grant{Role: auth.RolePlatformAdmin}), platform, "acme", ""},
		{"platform admin: any tenant", grants(auth.Grant{Role: auth.RolePlatformAdmin}), write, "globex", ""},
		{"tenant admin writes its tenant", grants(admin), write, "acme", ""},
		{"tenant admin reads its tenant", grants(admin), read, "acme", ""},
		{"tenant admin: another tenant", grants(admin), read, "globex", "no access to tenant globex"},
		{"tenant admin: platform call", grants(admin), platform, "acme", "needs the PlatformAdmin role"},
		{"viewer reads", grants(viewer), read, "acme", ""},
		{"viewer writes", grants(viewer), write, "acme", "read-only"},
		{"viewer of one, admin of another", grants(viewer, auth.Grant{Role: auth.RoleTenantAdmin, Tenants: []string{"globex"}}), write, "globex", ""},
		{"viewer and admin of the same tenant", grants(viewer, admin), write, "acme", ""},
		{"wildcard viewer reads any tenant", grants(auth.Grant{Role: auth.RoleTenantViewer, Tenants: []string{"*"}}), read, "globex", ""},
		{"wildcard viewer writes", grants(auth.Grant{Role: auth.RoleTenantViewer, Tenants: []string{"*"}}), write, "globex", "read-only"},
		{"no grants: tenant call", grants(), read, "acme", "no access to tenant acme"},
		{"no grants: open call", grants(), anyone, "", ""},
		{"no tenant in the request", grants(admin), read, "", "names no tenant"},
		{"unclassified scope", grants(admin), auth.Access{}, "acme", "not open"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.id.Authorize(tt.access, tt.tenant)

			switch {
			case tt.deny == "" && err != nil:
				t.Fatalf("denied: %v", err)
			case tt.deny != "" && (!fault.Is(err, fault.PermissionDenied) || !strings.Contains(err.Error(), tt.deny)):
				t.Fatalf("Authorize = %v, want PermissionDenied mentioning %q", err, tt.deny)
			}
		})
	}
}

func TestIdentityTravelsInTheContext(t *testing.T) {
	if id := auth.FromContext(context.Background()); !id.Anonymous {
		t.Errorf("a context without a caller = %+v, want anonymous", id)
	}
	want := &auth.Identity{Subject: "u1"}

	got := auth.FromContext(auth.WithIdentity(context.Background(), want))

	if got != want {
		t.Errorf("FromContext = %+v", got)
	}
}
