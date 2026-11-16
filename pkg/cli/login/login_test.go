package login_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/auth/authtest"
	"github.com/yindia/cluster-vending/pkg/cli/login"
)

const clientID = "cluster-vending-cli"

// idp is a fake issuer with the OAuth endpoints the CLI uses. It answers
// the device grant after pending polls, the authorization code grant when
// the PKCE verifier matches the challenge, and the refresh grant.
type idp struct {
	*authtest.Issuer
	t  testing.TB
	mu sync.Mutex
	// pending is how many polls answer authorization_pending first.
	pending int
	// deny makes the device grant end with access_denied.
	deny bool
	// challenge is the PKCE challenge the last /authorize saw; grants the
	// grant types the token endpoint was asked for, forms their forms.
	challenge string
	grants    []string
	forms     []url.Values
	// refreshFails makes the refresh grant fail.
	refreshFails           bool
	nonce                  string
	lastIDToken            string
	changeClaims           func(map[string]any)
	clientSecret           string
	basicUser, basicSecret string
	refreshID              bool
	wrongSigner            bool
	omitID                 bool
	exchangeError          string
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	i := &idp{Issuer: authtest.New(t), t: t}
	i.Mux.HandleFunc("POST /device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		i.record(r.Form, "device_authorization")
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code": "device-1", "user_code": "ABCD-EFGH", "verification_uri": i.URL + "/activate",
			"expires_in": 600, "interval": 1})
	})
	i.Mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		i.mu.Lock()
		i.challenge = q.Get("code_challenge")
		i.nonce = q.Get("nonce")
		i.mu.Unlock()
		back, _ := url.Parse(q.Get("redirect_uri"))
		back.RawQuery = url.Values{"code": {"code-1"}, "state": {q.Get("state")}}.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	i.Mux.HandleFunc("POST /token", i.token)
	return i
}

func (i *idp) record(form url.Values, grant string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.grants, i.forms = append(i.grants, grant), append(i.forms, form)
}

func (i *idp) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	grant := r.Form.Get("grant_type")
	i.record(r.Form, grant)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.basicUser, i.basicSecret, _ = r.BasicAuth()
	i.basicUser, _ = url.QueryUnescape(i.basicUser)
	i.basicSecret, _ = url.QueryUnescape(i.basicSecret)
	if i.clientSecret != "" && (i.basicUser != clientID || i.basicSecret != i.clientSecret) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client", "error_description": "rejected " + i.clientSecret})
		return
	}
	switch grant {
	case "urn:ietf:params:oauth:grant-type:device_code":
		if i.deny {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "access_denied"})
			return
		}
		if i.pending > 0 {
			i.pending--
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "authorization_pending"})
			return
		}
	case "authorization_code":
		if i.exchangeError != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": i.exchangeError, "error_description": "private-provider-description code-1 access-1"})
			return
		}
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != "code-1" || base64.RawURLEncoding.EncodeToString(sum[:]) != i.challenge {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
	case "refresh_token":
		if i.refreshFails || r.Form.Get("refresh_token") != "refresh-1" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		answer := map[string]any{"token_type": "Bearer", "access_token": "access-2", "refresh_token": "refresh-2", "expires_in": 3600}
		if i.refreshID {
			answer["id_token"] = i.signedID()
		}
		writeJSON(w, http.StatusOK, answer)
		return
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token_type": "Bearer", "access_token": "access-1",
		"refresh_token": "refresh-1", "id_token": i.signedID(), "expires_in": 3600})
}

// Called with i.mu held, just like the token endpoint state it models.
func (i *idp) signedID() string {
	if i.omitID {
		return ""
	}
	claims := i.Claims(clientID, "cli-user", map[string]any{"nonce": i.nonce})
	if i.changeClaims != nil {
		i.changeClaims(claims)
	}
	if i.wrongSigner {
		i.lastIDToken = i.SignWithOtherKey(i.t, claims)
	} else {
		i.lastIDToken = i.Sign(i.t, claims)
	}
	return i.lastIDToken
}

func (i *idp) issuedID() string { i.mu.Lock(); defer i.mu.Unlock(); return i.lastIDToken }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (i *idp) seen() ([]string, []url.Values) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.grants), slices.Clone(i.forms)
}

// The device flow shows the URL and the code, polls while the user is
// busy, and ends with the issuer's tokens.
func TestLoginWithADeviceCode(t *testing.T) {
	i := newIDP(t)
	i.pending = 1
	var out bytes.Buffer

	creds, err := login.Login(t.Context(), login.Options{Issuer: i.URL, ClientID: clientID, Out: &out})

	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "access-1" || creds.RefreshToken != "refresh-1" || creds.IDToken != i.issuedID() || creds.Bearer() != "access-1" {
		t.Errorf("credentials = %+v", creds)
	}
	if creds.Issuer != i.URL || creds.ClientID != clientID || creds.TokenURL != i.URL+"/token" || creds.Expiry.Before(time.Now()) {
		t.Errorf("credentials lack what a refresh needs: %+v", creds)
	}
	if !slices.Equal(creds.Scopes, []string{"openid", "offline_access"}) {
		t.Errorf("scopes = %v, want the defaults", creds.Scopes)
	}
	if !strings.Contains(out.String(), i.URL+"/activate") || !strings.Contains(out.String(), "ABCD-EFGH") {
		t.Errorf("the user was not told where to go and what to enter:\n%s", out.String())
	}
	grants, forms := i.seen()
	if len(grants) != 3 || grants[0] != "device_authorization" || forms[0].Get("client_id") != clientID || forms[0].Get("scope") != "openid offline_access" {
		t.Errorf("requests = %v %v, want a device authorization and two polls", grants, forms)
	}
	for _, f := range forms {
		if f.Get("client_secret") != "" {
			t.Error("a public client sent a secret")
		}
	}
}

func TestLoginWithADeviceCodeRefused(t *testing.T) {
	i := newIDP(t)
	i.deny = true

	_, err := login.Login(t.Context(), login.Options{Issuer: i.URL, ClientID: clientID})

	if err == nil || !strings.Contains(err.Error(), "login not completed") || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("Login = %v, want the issuer's refusal", err)
	}
}

// The browser flow sends the user to the issuer with a PKCE challenge and
// a state, takes the code on a loopback port, and proves the verifier.
func TestLoginThroughTheBrowser(t *testing.T) {
	i := newIDP(t)
	var out bytes.Buffer
	var opened string
	open := func(target string) error {
		opened = target
		res, err := http.Get(target) // the browser: follows the issuer's redirect back to the CLI
		if err != nil {
			return err
		}
		return res.Body.Close()
	}

	creds, err := login.Login(t.Context(), login.Options{Issuer: i.URL, ClientID: clientID, Browser: true,
		UseIDToken: true, Scopes: []string{"openid", "profile"}, Out: &out, OpenBrowser: open, RedirectHost: "127.0.0.1"})

	if err != nil {
		t.Fatal(err)
	}
	if creds.Bearer() != i.issuedID() || !creds.UseIDToken {
		t.Errorf("credentials = %+v, want the ID token as bearer", creds)
	}
	u, _ := url.Parse(opened)
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || len(q.Get("state")) < 16 ||
		q.Get("response_type") != "code" || q.Get("scope") != "openid profile" {
		t.Errorf("authorization request %s lacks PKCE, state or scopes", opened)
	}
	if redirect, _ := url.Parse(q.Get("redirect_uri")); redirect.Hostname() != "127.0.0.1" || redirect.Path != "/callback" {
		t.Errorf("redirect_uri = %s, want a loopback callback", q.Get("redirect_uri"))
	}
	if !strings.Contains(out.String(), opened) {
		t.Error("the URL was not printed for a user without a browser on this machine")
	}
}

func TestLoginThroughTheBrowserFails(t *testing.T) {
	i := newIDP(t)
	callbackOf := func(authURL string) *url.URL {
		u, _ := url.Parse(authURL)
		back, _ := url.Parse(u.Query().Get("redirect_uri"))
		return back
	}
	get := func(u *url.URL) error {
		res, err := http.Get(u.String())
		if err != nil {
			return err
		}
		return res.Body.Close()
	}
	tests := []struct {
		name string
		open func(authURL string) error
		want string
	}{
		{"the issuer refuses", func(authURL string) error {
			u, _ := url.Parse(authURL)
			back := callbackOf(authURL)
			back.RawQuery = url.Values{"error": {"access_denied"}, "state": {u.Query().Get("state")}}.Encode()
			return get(back)
		}, "access_denied"},
		{"no code", func(authURL string) error {
			u, _ := url.Parse(authURL)
			back := callbackOf(authURL)
			back.RawQuery = url.Values{"state": {u.Query().Get("state")}}.Encode()
			return get(back)
		}, "without a code"},
		{"a code the issuer does not know", func(authURL string) error {
			u, _ := url.Parse(authURL)
			back := callbackOf(authURL)
			back.RawQuery = url.Values{"code": {"stolen"}, "state": {u.Query().Get("state")}}.Encode()
			return get(back)
		}, "invalid_grant"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := login.Login(t.Context(), login.Options{Issuer: i.URL, ClientID: clientID, Browser: true,
				OpenBrowser: tt.open, RedirectHost: "127.0.0.1"})

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Login = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
}

// Without a browser the login waits; it ends when the context does.
func TestLoginThroughTheBrowserEndsWithTheContext(t *testing.T) {
	i := newIDP(t)
	ctx, cancel := context.WithCancel(t.Context())
	var out bytes.Buffer
	open := func(string) error {
		cancel()
		return errors.New("no browser here")
	}

	_, err := login.Login(ctx, login.Options{Issuer: i.URL, ClientID: clientID, Browser: true, Out: &out, OpenBrowser: open})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Login = %v, want the context's end", err)
	}
	if !strings.Contains(out.String(), "could not open the browser") {
		t.Errorf("the failed browser was not mentioned:\n%s", out.String())
	}
}

func TestLoginOptions(t *testing.T) {
	const tenant = "11111111-2222-3333-4444-555555555555"
	tests := []struct {
		name string
		opts login.Options
		want string
	}{
		{"nothing", login.Options{}, "--issuer and --client-id are required"},
		{"issuer without a client", login.Options{Issuer: "https://login.example.com"}, "--issuer and --client-id are required"},
		{"entra and an issuer", login.Options{Issuer: "https://login.example.com", EntraTenant: tenant}, "not both"},
		{"entra without a scope", login.Options{EntraTenant: tenant, EntraClientID: "c"}, "are all required"},
		{"entra tenant by name", login.Options{EntraTenant: "contoso.onmicrosoft.com", EntraClientID: "c", EntraScope: "api://a/.default"}, "a GUID"},
		{"id token without openid", login.Options{Issuer: "https://login.example.com", ClientID: "c", UseIDToken: true, Scopes: []string{"profile"}}, "needs the openid scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := login.Login(t.Context(), tt.opts)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Login = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
}

// The Entra flags stand for the tenant's v2.0 issuer and the API's scope
// plus offline_access. The issuer is Microsoft's, so the login itself is
// stopped at the first request: what matters is where it went.
func TestLoginEntraPreset(t *testing.T) {
	const tenant = "11111111-2222-3333-4444-555555555555"
	var asked []string
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.String())
		return nil, errors.New("no network in tests")
	})}

	_, err := login.Login(t.Context(), login.Options{EntraTenant: strings.ToUpper(tenant), EntraClientID: "cli-app",
		EntraScope: "api://cluster-vending/.default", HTTPClient: client})

	if err == nil || !strings.Contains(err.Error(), "read the issuer's configuration") {
		t.Fatalf("Login = %v", err)
	}
	want := "https://login.microsoftonline.com/" + tenant + "/v2.0/.well-known/openid-configuration"
	if !slices.Equal(asked, []string{want}) {
		t.Errorf("requests = %v, want the tenant's v2.0 discovery document", asked)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// An issuer without a device endpoint points the user at --browser.
func TestLoginWithoutADeviceEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"issuer": srv.URL, "jwks_uri": srv.URL + "/keys",
			"authorization_endpoint": srv.URL + "/authorize", "token_endpoint": srv.URL + "/token"})
	})

	_, err := login.Login(t.Context(), login.Options{Issuer: srv.URL, ClientID: clientID})

	if err == nil || !strings.Contains(err.Error(), "--browser") {
		t.Fatalf("Login = %v, want a pointer to --browser", err)
	}
}

func storeIn(t *testing.T) login.Store {
	t.Helper()
	return login.Store{Path: filepath.Join(t.TempDir(), "cluster-vending", "token.json")}
}

func TestStoreKeepsTheLoginPrivate(t *testing.T) {
	store := storeIn(t)
	if got, err := store.Load(); got != nil || err != nil {
		t.Fatalf("an empty store = %+v, %v", got, err)
	}
	want := &login.Credentials{Issuer: "https://login.example.com", ClientID: clientID, TokenURL: "https://login.example.com/token",
		Scopes: []string{"openid"}, AccessToken: "a", IDToken: "i", RefreshToken: "r", Expiry: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}

	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(want); err != nil {
		t.Fatalf("saving over a login: %v", err)
	}
	got, err := store.Load()

	if err != nil || got.AccessToken != "a" || got.RefreshToken != "r" || !got.Expiry.Equal(want.Expiry) || got.Bearer() != "a" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		file, _ := os.Stat(store.Path)
		dir, _ := os.Stat(filepath.Dir(store.Path))
		if file.Mode().Perm() != 0o600 || dir.Mode().Perm() != 0o700 {
			t.Errorf("modes: file %04o, directory %04o; want 0600 and 0700", file.Mode().Perm(), dir.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(store.Path))
	if len(entries) != 1 {
		t.Errorf("%d files next to the login, want no temporary file left", len(entries))
	}

	removed, err := store.Delete()
	again, againErr := store.Delete()
	if !removed || err != nil || again || againErr != nil {
		t.Errorf("Delete = %t, %v; again %t, %v", removed, err, again, againErr)
	}
}

func TestStoreRefusesAFileOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no permission bits")
	}
	store := storeIn(t)
	if err := store.Save(&login.Credentials{AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.Path, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := store.Load()

	if err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("Load = %v, want a refusal", err)
	}
}

func TestStoreRejectsAnotherFile(t *testing.T) {
	store := storeIn(t)
	if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Load(); err == nil || !strings.Contains(err.Error(), "not a login file") {
		t.Fatalf("Load = %v", err)
	}
}

func TestDefaultStoreIsInTheUsersConfigDirectory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())

	store, err := login.DefaultStore()

	if err != nil || filepath.Base(store.Path) != "token.json" || filepath.Base(filepath.Dir(store.Path)) != "cluster-vending" {
		t.Fatalf("DefaultStore = %+v, %v", store, err)
	}
}
