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
	if got.UsernameClaim != "sub" || len(got.Alternates) != 0 {
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
			if got.UsernameClaim != "oid" {
				t.Errorf("username claim = %s, want oid", got.UsernameClaim)
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

func TestResolvedKeepsAnExplicitUsernameClaim(t *testing.T) {
	issuer := generic()
	issuer.UsernameClaim = "email"

	cfg, err := auth.Config{Mode: auth.ModeRequired, Issuers: []auth.Issuer{issuer}}.Resolved()

	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Issuers[0]; got.UsernameClaim != "email" {
		t.Errorf("claims = %+v", got)
	}
}

// The keys of the removed roles still load, each ignored with a warning,
// so an older file does not stop the server.
func TestResolvedIgnoresTheRemovedRoleKeysWithAWarning(t *testing.T) {
	issuer := generic()
	issuer.LegacyTenantClaim, issuer.LegacyRolesClaim = "org", "groups"
	cfg, err := auth.Config{Mode: auth.ModeRequired, Issuers: []auth.Issuer{issuer},
		LegacyRules: []any{map[string]any{"match": map[string]any{"issuer": "keycloak"}}}}.Resolved()

	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Ignored) != 3 {
		t.Fatalf("ignored = %q, want a warning for rules, tenantClaim and rolesClaim", cfg.Ignored)
	}
	for i, want := range []string{"auth.rules is ignored", "auth.issuers[0].tenantClaim is ignored", "auth.issuers[0].rolesClaim is ignored"} {
		if !strings.Contains(cfg.Ignored[i], want) {
			t.Errorf("warning %d = %q, want %q", i, cfg.Ignored[i], want)
		}
	}
	if cfg.LegacyRules != nil || cfg.Issuers[0].LegacyTenantClaim != "" || cfg.Issuers[0].LegacyRolesClaim != "" {
		t.Errorf("the resolved configuration keeps an ignored key: %+v", cfg)
	}
}

func TestResolvedRefusesTheRemovedOptionalMode(t *testing.T) {
	_, err := auth.Config{Mode: "optional", Issuers: []auth.Issuer{generic()}}.Resolved()

	if err == nil || !strings.Contains(err.Error(), "optional is removed") || !strings.Contains(err.Error(), "required") {
		t.Fatalf("optional = %v, want a refusal naming required", err)
	}
}

func TestResolvedReportsProblems(t *testing.T) {
	tests := []struct {
		name string
		cfg  auth.Config
		want []string
	}{
		{"unknown mode", auth.Config{Mode: "strict"}, []string{"auth.mode must be none or required"}},
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
		{"all at once", auth.Config{Mode: "x", Issuers: []auth.Issuer{{Name: "a", Issuer: "https://x.example.com"}}},
			[]string{"auth.mode", "audiences"}},
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
	ok := auth.Config{Mode: auth.ModeRequired, Issuers: []auth.Issuer{generic(), {Name: "dev", Issuer: "http://localhost:8081/realms/x", Audiences: []string{"a"}}}}
	if _, err := ok.Resolved(); err != nil {
		t.Errorf("a loopback issuer is valid: %v", err)
	}
}
