package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/config"
)

const authFile = `
http-addr: "127.0.0.1:9090"
auth:
  mode: required
  issuers:
  - name: entra
    audiences: ["99999999-8888-7777-6666-555555555555", "api://cluster-vending"]
    entra: {tenantId: 11111111-2222-3333-4444-555555555555, acceptV1: true}
  - name: keycloak
    issuer: https://login.example.com/realms/platform
    audiences: [cluster-vending]
    usernameClaim: preferred_username
    tenantClaim: org
    rolesClaim: groups
  rules:
  - {match: {issuer: entra, role: Platform.Admin}, grant: {role: PlatformAdmin}}
  - {match: {issuer: entra, role: Acme.Admin}, grant: {role: TenantAdmin, tenants: [acme]}}
  - {match: {issuer: keycloak}, grant: {role: TenantViewer}}
`

// The auth section loads next to the flat settings, which ignore it.
func TestLoadAuthNextToTheSettings(t *testing.T) {
	path := writeYAML(t, authFile)

	cfg, err := config.LoadAuth(path)

	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != auth.ModeRequired || len(cfg.Issuers) != 2 || len(cfg.Rules) != 3 {
		t.Fatalf("auth = %+v", cfg)
	}
	entra, keycloak := cfg.Issuers[0], cfg.Issuers[1]
	if entra.Issuer != "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0" ||
		entra.RolesClaim != "roles" || len(entra.Alternates) != 1 || len(entra.Audiences) != 2 {
		t.Errorf("entra issuer = %+v", entra)
	}
	if keycloak.UsernameClaim != "preferred_username" || keycloak.TenantClaim != "org" || keycloak.RolesClaim != "groups" {
		t.Errorf("keycloak issuer = %+v", keycloak)
	}
	if got := cfg.Rules[1].Grant; got.Role != auth.RoleTenantAdmin || !slices.Equal(got.Tenants, []string{"acme"}) {
		t.Errorf("rule 1 grants %+v", got)
	}
	file, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("the settings must not trip over the auth section: %v", err)
	}
	if serve, err := config.LoadServe(nil, lookup(nil), file); err != nil || serve.HTTPAddr != "127.0.0.1:9090" {
		t.Errorf("LoadServe = %+v, %v", serve, err)
	}
}

func TestAuthIsOffWithoutTheSection(t *testing.T) {
	tests := []struct {
		name  string
		flags map[string]string
		env   map[string]string
	}{
		{"no file", nil, nil},
		{"a file without the section (flag)", map[string]string{config.FlagConfig: writeYAML(t, "log-level: debug\n")}, nil},
		{"an empty file (environment)", nil, map[string]string{config.EnvConfig: writeYAML(t, "")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.AuthFrom(lookup(tt.flags), lookup(tt.env))

			if err != nil || cfg.Mode != auth.ModeNone || len(cfg.Issuers) != 0 {
				t.Fatalf("AuthFrom = %+v, %v; want mode none", cfg, err)
			}
		})
	}
}

func TestAuthFromReadsTheConfigFile(t *testing.T) {
	path := writeYAML(t, authFile)

	fromFlag, err := config.AuthFrom(lookup(map[string]string{config.FlagConfig: path}), lookup(nil))
	fromEnv, envErr := config.AuthFrom(lookup(nil), lookup(map[string]string{config.EnvConfig: path}))

	if err != nil || envErr != nil || fromFlag.Mode != auth.ModeRequired || fromEnv.Mode != auth.ModeRequired {
		t.Fatalf("AuthFrom = %+v (%v), %+v (%v)", fromFlag, err, fromEnv, envErr)
	}
}

func TestLoadAuthRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"an unknown key", "auth:\n  mode: none\n  issuer: []\n", []string{"auth", "issuer"}},
		{"a misspelled issuer key", "auth:\n  issuers:\n  - {name: a, issuer: https://x.example.com, audience: [a]}\n", []string{"audience"}},
		{"a secret-looking key", "auth:\n  issuers:\n  - {name: a, issuer: https://x.example.com, audiences: [a], clientSecret: s}\n", []string{"clientSecret"}},
		{"every problem at once", "auth:\n  mode: strict\n  issuers:\n  - {name: a, issuer: https://x.example.com}\n  rules:\n  - {match: {issuer: b}, grant: {role: Owner}}\n",
			[]string{"invalid configuration", "auth.mode", "audiences", "match.issuer", "grant.role"}},
		{"invalid yaml", "auth: [\n", []string{"invalid yaml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.LoadAuth(writeYAML(t, tt.body))

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
	if _, err := config.LoadAuth("/does/not/exist.yaml"); err == nil {
		t.Error("a missing file was accepted")
	}
}

// Authentication has no flag and no environment variable: it is lists, and
// a rule must not be changeable by whoever can set a variable.
func TestAuthHasNoFlags(t *testing.T) {
	for _, s := range slices.Concat(config.GlobalSettings(), config.ServeSettings()) {
		if strings.HasPrefix(s.Flag, config.SectionAuth) {
			t.Errorf("%s: authentication is configured in the settings file only", s)
		}
	}
}
