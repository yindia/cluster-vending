// Package authtest is a fake OIDC issuer for tests: an HTTP server with a
// discovery document and a key set, and tokens signed with its key (or
// deliberately not).
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const keyID = "test-key"

var (
	keyOnce   sync.Once
	sharedKey *rsa.PrivateKey
)

// key is one RSA key for every fake issuer of the test binary: generating
// one per test is slow.
func key(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		sharedKey = k
	})
	return sharedKey
}

// Issuer is a fake OIDC issuer.
type Issuer struct {
	// URL is where it listens: the base of its discovery document.
	URL string
	// Name is its "iss": URL, unless made with NewNamed.
	Name string
	// Mux serves it; a test adds the endpoints it needs (/token, /device,
	// /authorize are advertised by the discovery document).
	Mux *http.ServeMux

	mu          sync.Mutex
	discoveries int
	down        bool
	signer      *rsa.PrivateKey
}

// PublicKey is the key the issuer publishes.
func (i *Issuer) PublicKey() *rsa.PublicKey { return &i.signer.PublicKey }

// Discoveries is how often the discovery document was read.
func (i *Issuer) Discoveries() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.discoveries
}

// SetDown makes the discovery document fail (503) or work again.
func (i *Issuer) SetDown(down bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.down = down
}

// New starts an issuer whose "iss" is its own URL.
func New(t testing.TB) *Issuer { return NewNamed(t, "") }

// NewNamed starts an issuer whose discovery document names another
// issuer than its URL, as Entra's v1.0 document does; "" for its URL.
func NewNamed(t testing.TB, name string) *Issuer {
	t.Helper()
	i := &Issuer{Mux: http.NewServeMux(), signer: key(t)}
	srv := httptest.NewServer(i.Mux)
	t.Cleanup(srv.Close)
	i.URL, i.Name = srv.URL, name
	if i.Name == "" {
		i.Name = srv.URL
	}
	i.Mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		i.mu.Lock()
		i.discoveries++
		down := i.down
		i.mu.Unlock()
		if down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{
			"issuer":                                i.Name,
			"jwks_uri":                              i.URL + "/keys",
			"authorization_endpoint":                i.URL + "/authorize",
			"token_endpoint":                        i.URL + "/token",
			"device_authorization_endpoint":         i.URL + "/device",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	i.Mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &i.signer.PublicKey, KeyID: keyID, Algorithm: "RS256", Use: "sig"}}})
	})
	return i
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Claims are the claims of a token of this issuer for audience and
// subject, valid for an hour; extra adds to and overrides them.
func (i *Issuer) Claims(audience, subject string, extra map[string]any) map[string]any {
	now := time.Now()
	claims := map[string]any{
		"iss": i.Name, "aud": audience, "sub": subject,
		"iat": now.Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	return claims
}

// Sign is a token with claims, signed with the issuer's key (RS256).
func (i *Issuer) Sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	return sign(t, jose.SigningKey{Algorithm: jose.RS256, Key: i.signer}, claims)
}

// SignWithOtherKey is a token signed with a key the issuer does not
// publish.
func (i *Issuer) SignWithOtherKey(t testing.TB, claims map[string]any) string {
	t.Helper()
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return sign(t, jose.SigningKey{Algorithm: jose.RS256, Key: other}, claims)
}

// SignHS256 is a token signed with a shared secret: the algorithm
// confusion attack signs with the issuer's public key as that secret.
func SignHS256(t testing.TB, secret []byte, claims map[string]any) string {
	t.Helper()
	return sign(t, jose.SigningKey{Algorithm: jose.HS256, Key: secret}, claims)
}

func sign(t testing.TB, k jose.SigningKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(k, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", keyID))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return token
}

// Unsigned is a token with alg "none" and no signature.
func Unsigned(t testing.TB, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + "."
}
