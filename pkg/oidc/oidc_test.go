package oidc_test

import (
	"reflect"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/oidc"
)

const (
	tenantID = "833ced3d-cb2e-41de-92f1-29e2af035ddc"
	clientID = "00001111-aaaa-2222-bbbb-3333cccc4444"
)

func TestResolve(t *testing.T) {
	entra := &cloudv1.EntraIdentityProvider{TenantId: tenantID, ClientId: clientID}
	tests := []struct {
		name string
		spec *cloudv1.OidcIdentityProvider
		want *oidc.Provider
	}{
		{name: "none"},
		{
			name: "entra fills in the issuer and the claims",
			spec: &cloudv1.OidcIdentityProvider{Entra: entra},
			want: &oidc.Provider{Name: "entra", Entra: true, ClientID: clientID,
				IssuerURL:     "https://login.microsoftonline.com/" + tenantID + "/v2.0",
				UsernameClaim: "oid", UsernamePrefix: "entra:", GroupsClaim: "groups", GroupsPrefix: "entra:"},
		},
		{
			name: "entra keeps its own name and required claims",
			spec: &cloudv1.OidcIdentityProvider{Name: "corp", Entra: entra, RequiredClaims: map[string]string{"tid": tenantID}},
			want: &oidc.Provider{Name: "corp", Entra: true, ClientID: clientID,
				IssuerURL:     "https://login.microsoftonline.com/" + tenantID + "/v2.0",
				UsernameClaim: "oid", UsernamePrefix: "entra:", GroupsClaim: "groups", GroupsPrefix: "entra:",
				RequiredClaims: map[string]string{"tid": tenantID}},
		},
		{
			name: "a generic provider is taken as it is",
			spec: &cloudv1.OidcIdentityProvider{Name: "okta", IssuerUrl: "https://example.okta.com", ClientId: "kubernetes",
				UsernameClaim: "email", GroupsClaim: "groups", GroupsPrefix: "oidc:"},
			want: &oidc.Provider{Name: "okta", IssuerURL: "https://example.okta.com", ClientID: "kubernetes",
				UsernameClaim: "email", GroupsClaim: "groups", GroupsPrefix: "oidc:"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := oidc.Resolve(tt.spec)

			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Resolve = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveDoesNotShareTheRequiredClaims(t *testing.T) {
	spec := &cloudv1.OidcIdentityProvider{Name: "okta", IssuerUrl: "https://example.okta.com", ClientId: "kubernetes",
		RequiredClaims: map[string]string{"hd": "example.com"}}

	oidc.Resolve(spec).RequiredClaims["hd"] = "changed"

	if spec.GetRequiredClaims()["hd"] != "example.com" {
		t.Fatal("Resolve handed out the spec's own map")
	}
}

func TestScopes(t *testing.T) {
	tests := []struct {
		name string
		p    oidc.Provider
		want []string
	}{
		{"entra needs profile for oid, and a refresh token", oidc.Provider{Entra: true, UsernameClaim: "oid", GroupsClaim: "groups"},
			[]string{"profile", "offline_access"}},
		{"sub needs nothing", oidc.Provider{}, nil},
		{"email", oidc.Provider{UsernameClaim: "email"}, []string{"email"}},
		{"preferred_username and groups", oidc.Provider{UsernameClaim: "preferred_username", GroupsClaim: "groups"}, []string{"groups", "profile"}},
		{"another groups claim has no scope of its own", oidc.Provider{UsernameClaim: "sub", GroupsClaim: "roles"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.p.Scopes()

			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Scopes = %v, want %v", got, tt.want)
			}
		})
	}
}
