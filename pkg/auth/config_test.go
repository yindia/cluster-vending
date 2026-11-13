package auth_test

import (
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/auth"
)

const entraTenant = "11111111-2222-3333-4444-555555555555"

func generic() auth.Issuer {
	return auth.Issuer{Name: "keycloak", Issuer: "https://login.example.com/realms/platform", Audiences: []string{"cluster-vending"}}
}

func TestResolvedDefaults(t *testing.T) {
	cfg, err := auth.Config{}.Resolved()

	if err != nil || cfg.Mode != auth.ModeNone {
		t.Fatalf("empty config = %+v, %v; want mode none", cfg, err)
	}
}

func TestResolvedAppliesIssuerDefaults(t *testing.T) {
	cfg, err := auth.Config{Mode: auth.ModeRequired, Issuers: []auth.Issuer{generic()}}.Resolved()

	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Issuers[0]
	if got.UsernameClaim != "sub" || got.RolesClaim != "groups" || len(got.Alternates) != 0 {
		t.Errorf("generic defaults = %+v", got)
	}
}

func TestResolvedAppliesTheEntraPreset(t *testing.T) {
	tests := []struct {
		name           string
		entra          auth.Entra
		wantAlternates []auth.Alternate
	}{
		{"v2 only", auth.Entra{TenantID: entraTenant}, nil},
		{"v1 too", auth.Entra{TenantID: strings.ToUpper(entraTenant), AcceptV1: true}, []auth.Alternate{{
			Issuer:       "https://sts.windows.net/" + entraTenant + "/",
			DiscoveryURL: "https://login.microsoftonline.com/" + entraTenant}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := auth.Config{Mode: auth.ModeRequired, Issuers: []auth.Issuer{{
				Name: "entra", Audiences: []string{"api://app"}, Entra: &tt.entra}}}

			cfg, err := in.Resolved()

			if err != nil {
				t.Fatal(err)
			}
			got := cfg.Issuers[0]
			if got.Issuer != "https://login.microsoftonline.com/"+entraTenant+"/v2.0" {
				t.Errorf("issuer = %s", got.Issuer)
			}
			if got.UsernameClaim != "oid" || got.RolesClaim != "roles" {
				t.Errorf("claims = %s, %s; want oid, roles", got.UsernameClaim, got.RolesClaim)
			}
			if len(got.Alternates) != len(tt.wantAlternates) || (len(got.Alternates) == 1 && got.Alternates[0] != tt.wantAlternates[0]) {
				t.Errorf("alternates = %+v, want %+v", got.Alternates, tt.wantAlternates)
			}
			if in.Issuers[0].Issuer != "" {
				t.Error("Resolved changed its input")
			}
		})
	}
}

func TestResolvedKeepsExplicitClaims(t *testing.T) {
	issuer := generic()
	issuer.UsernameClaim, issuer.RolesClaim, issuer.TenantClaim = "email", "roles", "org"

	cfg, err := auth.Config{Mode: auth.ModeOptional, Issuers: []auth.Issuer{issuer}}.Resolved()

	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Issuers[0]; got.UsernameClaim != "email" || got.RolesClaim != "roles" || got.TenantClaim != "org" {
		t.Errorf("claims = %+v", got)
	}
}

func TestResolvedReportsProblems(t *testing.T) {
	withClaim := generic()
	withClaim.TenantClaim = "org"
	rule := func(issuer string, role auth.Role, tenants ...string) auth.Rule {
		return auth.Rule{Match: auth.Match{Issuer: issuer}, Grant: auth.Grant{Role: role, Tenants: tenants}}
	}
	tests := []struct {
		name string
		cfg  auth.Config
		want []string
	}{
		{"unknown mode", auth.Config{Mode: "strict"}, []string{"auth.mode must be"}},
		{"required without issuers", auth.Config{Mode: auth.ModeRequired}, []string{"needs at least one issuer"}},
		{"issuer name", auth.Config{Issuers: []auth.Issuer{{Name: "Entra ID", Issuer: "https://x.example.com", Audiences: []string{"a"}}}},
			[]string{"auth.issuers[0]: name"}},
		{"issuer over http", auth.Config{Issuers: []auth.Issuer{{Name: "a", Issuer: "http://login.example.com", Audiences: []string{"a"}}}},
			[]string{"must use https"}},
		{"issuer not a url", auth.Config{Issuers: []auth.Issuer{{Name: "a", Issuer: "login", Audiences: []string{"a"}}}},
			[]string{"must be a URL"}},
		{"issuer with a query", auth.Config{Issuers: []auth.Issuer{{Name: "a", Issuer: "https://x.example.com/?p=1", Audiences: []string{"a"}}}},
			[]string{"must be a URL"}},
		{"no audiences", auth.Config{Issuers: []auth.Issuer{{Name: "a", Issuer: "https://x.example.com"}}},
			[]string{"audiences needs at least one"}},
		{"empty audience", auth.Config{Issuers: []auth.Issuer{{Name: "a", Issuer: "https://x.example.com", Audiences: []string{""}}}},
			[]string{"audiences has an empty value"}},
		{"entra tenant", auth.Config{Issuers: []auth.Issuer{{Name: "a", Audiences: []string{"a"}, Entra: &auth.Entra{TenantID: "contoso"}}}},
			[]string{"entra.tenantId"}},
		{"entra with another issuer", auth.Config{Issuers: []auth.Issuer{{Name: "a", Issuer: "https://login.example.com", Audiences: []string{"a"},
			Entra: &auth.Entra{TenantID: entraTenant}}}}, []string{"is not the tenant's"}},
		{"duplicate name", auth.Config{Issuers: []auth.Issuer{generic(), generic()}}, []string{"is used twice", "both accept issuer"}},
		{"rule for an unknown issuer", auth.Config{Issuers: []auth.Issuer{generic()}, Rules: []auth.Rule{rule("okta", auth.RolePlatformAdmin)}},
			[]string{"auth.rules[0]: match.issuer \"okta\""}},
		{"unknown role", auth.Config{Issuers: []auth.Issuer{generic()}, Rules: []auth.Rule{rule("keycloak", "Owner", "acme")}},
			[]string{"grant.role must be"}},
		{"platform admin with tenants", auth.Config{Issuers: []auth.Issuer{generic()}, Rules: []auth.Rule{rule("keycloak", auth.RolePlatformAdmin, "acme")}},
			[]string{"must be unset for PlatformAdmin"}},
		{"tenant role without tenants", auth.Config{Issuers: []auth.Issuer{generic()}, Rules: []auth.Rule{rule("keycloak", auth.RoleTenantAdmin)}},
			[]string{"grant.tenants needs the tenants"}},
		{"star with others", auth.Config{Issuers: []auth.Issuer{generic()}, Rules: []auth.Rule{rule("keycloak", auth.RoleTenantViewer, "*", "acme")}},
			[]string{"stands alone"}},
		{"bad tenant", auth.Config{Issuers: []auth.Issuer{generic()}, Rules: []auth.Rule{rule("keycloak", auth.RoleTenantViewer, "Acme_Corp")}},
			[]string{"is not a tenant ID"}},
		{"all at once", auth.Config{Mode: "x", Issuers: []auth.Issuer{{Name: "a", Issuer: "https://x.example.com"}},
			Rules: []auth.Rule{rule("b", "Owner")}}, []string{"auth.mode", "audiences", "match.issuer", "grant.role"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.cfg.Resolved()

			if err == nil {
				t.Fatal("accepted")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
	ok := auth.Config{Mode: auth.ModeRequired, Issuers: []auth.Issuer{withClaim, {Name: "dev", Issuer: "http://localhost:8081/realms/x", Audiences: []string{"a"}}},
		Rules: []auth.Rule{rule("keycloak", auth.RoleTenantAdmin), rule("keycloak", auth.RoleTenantViewer, "*"), rule("dev", auth.RolePlatformAdmin)}}
	if _, err := ok.Resolved(); err != nil {
		t.Errorf("a tenant claim, a wildcard and a loopback issuer are valid: %v", err)
	}
}
