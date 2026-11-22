package auth_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/auth/authtest"
	"github.com/yindia/cluster-vending/pkg/fault"
)

const audience = "api://cluster-vending"

// verifierFor is a Verifier of one issuer named "idp", and the issuer
// itself.
func verifierFor(t *testing.T, change func(*auth.Issuer), opts ...auth.VerifierOption) (*auth.Verifier, *authtest.Issuer) {
	t.Helper()
	idp := authtest.New(t)
	issuer := auth.Issuer{Name: "idp", Issuer: idp.URL, Audiences: []string{audience, "second-audience"}}
	if change != nil {
		change(&issuer)
	}
	cfg, err := auth.Config{Mode: auth.ModeRequired, Issuers: []auth.Issuer{issuer}}.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewVerifier(cfg, opts...), idp
}

func TestVerifyAcceptsAGoodToken(t *testing.T) {
	v, idp := verifierFor(t, nil)
	token := idp.Sign(t, idp.Claims(audience, "user-1", map[string]any{"roles": []string{"acme.admin", "acme.viewer", "unrelated"}}))

	id, err := v.Verify(t.Context(), token)

	if err != nil {
		t.Fatal(err)
	}
	if id.Anonymous || id.Subject != "user-1" || id.Username != "user-1" || id.Issuer != "idp" {
		t.Errorf("identity = %+v", id)
	}
}

func TestVerifyReadsTheConfiguredClaims(t *testing.T) {
	tests := []struct {
		name     string
		change   func(*auth.Issuer)
		claims   map[string]any
		wantUser string
	}{
		{"second audience", nil, map[string]any{"aud": "second-audience"}, "u"},
		{"several audiences in the token", nil, map[string]any{"aud": []string{"other", audience}}, "u"},
		{"username claim", func(i *auth.Issuer) { i.UsernameClaim = "preferred_username" },
			map[string]any{"preferred_username": "ada@example.com"}, "ada@example.com"},
		{"username claim missing falls back to sub", func(i *auth.Issuer) { i.UsernameClaim = "email" }, nil, "u"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, idp := verifierFor(t, tt.change)
			token := idp.Sign(t, idp.Claims(audience, "u", tt.claims))

			id, err := v.Verify(t.Context(), token)

			if err != nil {
				t.Fatal(err)
			}
			if id.Username != tt.wantUser {
				t.Errorf("identity = %+v, want user %s", id, tt.wantUser)
			}
		})
	}
}

func TestVerifyRejects(t *testing.T) {
	v, idp := verifierFor(t, nil)
	other := authtest.New(t)
	publicKey := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustPublicKey(t, idp)})
	good := idp.Claims(audience, "u", nil)
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"not a jwt", "not-a-token", "not a signed JWT"},
		{"payload is not base64", "a.!!!.c", "not a signed JWT"},
		{"payload is not json", "a.bm90IGpzb24.c", "not a signed JWT"},
		{"no issuer claim", idp.Sign(t, map[string]any{"aud": audience, "sub": "u", "exp": time.Now().Add(time.Hour).Unix()}), "not a signed JWT"},
		{"too long", strings.Repeat("a", 20<<10), "not a signed JWT"},
		{"another issuer", other.Sign(t, other.Claims(audience, "u", nil)), "issuer is not accepted"},
		{"wrong audience", idp.Sign(t, idp.Claims("api://another", "u", nil)), "not issued for this API"},
		{"expired", idp.Sign(t, idp.Claims(audience, "u", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})), "has expired"},
		{"not valid yet", idp.Sign(t, idp.Claims(audience, "u", map[string]any{"nbf": time.Now().Add(time.Hour).Unix()})), "not valid"},
		{"signed with another key", idp.SignWithOtherKey(t, good), "not valid"},
		{"alg none", authtest.Unsigned(t, good), "not valid"},
		{"hs256 with the public key as the secret", authtest.SignHS256(t, publicKey, good), "not valid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := v.Verify(t.Context(), tt.token)

			if id != nil || !fault.Is(err, fault.Unauthenticated) {
				t.Fatalf("Verify = %+v, %v; want Unauthenticated", id, err)
			}
			if f, _ := fault.As(err); !strings.Contains(f.Message, tt.want) {
				t.Errorf("message %q does not mention %q", f.Message, tt.want)
			}
			if f, _ := fault.As(err); strings.Contains(f.Message, tt.token) && tt.token != "not-a-token" {
				t.Error("the message for the caller repeats the token")
			}
		})
	}
}

func mustPublicKey(t *testing.T, idp *authtest.Issuer) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(idp.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// A token just past its expiry is still accepted: the clocks differ.
func TestVerifyAllowsClockSkew(t *testing.T) {
	v, idp := verifierFor(t, nil)
	justExpired := idp.Sign(t, idp.Claims(audience, "u", map[string]any{"exp": time.Now().Add(-10 * time.Second).Unix()}))

	if _, err := v.Verify(t.Context(), justExpired); err != nil {
		t.Errorf("expired 10s ago: %v, want accepted within the leeway", err)
	}
}

// Entra issues v2.0 tokens (issuer .../v2.0, audience the API's client ID)
// or v1.0 tokens (issuer sts.windows.net, audience the Application ID URI)
// depending on the API's app registration; with acceptV1 both are taken,
// each verified against its own discovery document.
func TestVerifyEntraV1AndV2(t *testing.T) {
	const tenant, clientID, appURI = "11111111-2222-3333-4444-555555555555", "99999999-8888-7777-6666-555555555555", "api://cluster-vending"
	v2 := authtest.New(t)
	v1Issuer := auth.EntraV1Issuer(tenant)
	v1 := authtest.NewNamed(t, v1Issuer)
	issuer := auth.Issuer{Name: "entra", Issuer: v2.URL, Audiences: []string{clientID, appURI},
		UsernameClaim: "oid",
		Alternates:    []auth.Alternate{{Issuer: v1Issuer, DiscoveryURL: v1.URL}}}
	v := auth.NewVerifier(auth.Config{Mode: auth.ModeRequired, Issuers: []auth.Issuer{issuer}})
	tests := []struct {
		name   string
		token  string
		reject bool
	}{
		{"v2: client ID audience", v2.Sign(t, v2.Claims(clientID, "pairwise-sub", map[string]any{
			"ver": "2.0", "oid": "object-id", "tid": tenant, "azp": "cli", "roles": []string{"Platform.Admin"}})), false},
		{"v1: application ID URI audience", v1.Sign(t, v1.Claims(appURI, "pairwise-sub", map[string]any{
			"ver": "1.0", "oid": "object-id", "tid": tenant, "appid": "cli", "roles": []string{"Platform.Admin"}})), false},
		{"v1 issuer signed by nobody's key", v1.SignWithOtherKey(t, v1.Claims(appURI, "s", nil)), true},
		{"another tenant's v1 issuer", v1.Sign(t, v1.Claims(appURI, "s", map[string]any{"iss": auth.EntraV1Issuer("another")})), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := v.Verify(t.Context(), tt.token)

			if tt.reject {
				if !fault.Is(err, fault.Unauthenticated) {
					t.Fatalf("Verify = %+v, %v; want Unauthenticated", id, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if id.Username != "object-id" || id.Issuer != "entra" {
				t.Errorf("identity = %+v, want the oid as username", id)
			}
		})
	}
}

// Without acceptV1 a v1.0 token of the same tenant is refused.
func TestVerifyRefusesEntraV1WhenNotAccepted(t *testing.T) {
	v, _ := verifierFor(t, nil)
	v1 := authtest.NewNamed(t, auth.EntraV1Issuer("11111111-2222-3333-4444-555555555555"))

	_, err := v.Verify(t.Context(), v1.Sign(t, v1.Claims(audience, "u", nil)))

	if !fault.Is(err, fault.Unauthenticated) {
		t.Fatalf("Verify = %v, want Unauthenticated", err)
	}
}

// The issuer is read on the first token, once; while it is down tokens
// are refused as Unavailable without asking it for every request, and
// accepted again when it is back.
func TestVerifyDiscoversLazilyAndRetries(t *testing.T) {
	now := time.Now()
	v, idp := verifierFor(t, nil, auth.WithClock(func() time.Time { return now }), auth.WithHTTPClient(&http.Client{Timeout: 5 * time.Second}))
	token := idp.Sign(t, idp.Claims(audience, "u", nil))
	if idp.Discoveries() != 0 {
		t.Fatal("the issuer was read before any token arrived")
	}
	idp.SetDown(true)

	_, err := v.Verify(t.Context(), token)
	_, again := v.Verify(t.Context(), token)

	if !fault.Is(err, fault.Unavailable) || !fault.Is(again, fault.Unavailable) {
		t.Fatalf("issuer down: %v, %v; want Unavailable", err, again)
	}
	if idp.Discoveries() != 1 {
		t.Errorf("%d discoveries while down, want 1 (the failure is remembered)", idp.Discoveries())
	}

	idp.SetDown(false)
	if _, err := v.Verify(t.Context(), token); !fault.Is(err, fault.Unavailable) {
		t.Errorf("before the retry interval: %v, want still Unavailable", err)
	}
	now = now.Add(time.Minute)
	if _, err := v.Verify(t.Context(), token); err != nil {
		t.Fatalf("issuer back: %v", err)
	}
	if _, err := v.Verify(t.Context(), token); err != nil || idp.Discoveries() != 2 {
		t.Errorf("after recovery: %v, %d discoveries; want the document read once more and kept", err, idp.Discoveries())
	}
}

// A caller that goes away does not break verification for the next one.
func TestVerifyWithACanceledContext(t *testing.T) {
	v, idp := verifierFor(t, nil)
	token := idp.Sign(t, idp.Claims(audience, "u", nil))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _ = v.Verify(ctx, token)

	if _, err := v.Verify(t.Context(), token); err != nil {
		t.Errorf("after a canceled call: %v", err)
	}
}
