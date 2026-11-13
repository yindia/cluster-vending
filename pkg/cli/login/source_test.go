package login_test

import (
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/cli/login"
)

func noEnv(string) (string, bool) { return "", false }

// stored saves a login of issuer i that expires at expiry.
func stored(t *testing.T, i *idp, expiry time.Time, change func(*login.Credentials)) login.Store {
	t.Helper()
	store := storeIn(t)
	creds := &login.Credentials{Issuer: i.URL, ClientID: clientID, TokenURL: i.URL + "/token", Scopes: []string{"openid", "offline_access"},
		AccessToken: "access-1", IDToken: "id-1", RefreshToken: "refresh-1", Expiry: expiry}
	if change != nil {
		change(creds)
	}
	if err := store.Save(creds); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestTokenSource(t *testing.T) {
	soon := time.Now().Add(10 * time.Second) // inside the slack: as good as expired
	later := time.Now().Add(time.Hour)
	tests := []struct {
		name        string
		env         string
		login       bool
		expiry      time.Time
		change      func(*login.Credentials)
		want        string
		wantRefresh bool
		wantErr     string
	}{
		{name: "nobody logged in: no token, no error"},
		{name: "the environment alone", env: "from-env", want: "from-env"},
		{name: "the environment wins over the login", env: "from-env", login: true, expiry: later, want: "from-env"},
		{name: "a valid login", login: true, expiry: later, want: "access-1"},
		{name: "a login that does not say when it expires", login: true, want: "access-1"},
		{name: "the ID token when the login asks for it", login: true, expiry: later,
			change: func(c *login.Credentials) { c.UseIDToken = true }, want: "id-1"},
		{name: "an expired login is refreshed", login: true, expiry: soon, want: "access-2", wantRefresh: true},
		{name: "expired without a refresh token", login: true, expiry: soon,
			change: func(c *login.Credentials) { c.RefreshToken = "" }, wantErr: "run `cluster-vending login` again"},
		{name: "a refresh token the issuer no longer takes", login: true, expiry: soon,
			change: func(c *login.Credentials) { c.RefreshToken = "revoked" }, wantRefresh: true, wantErr: "could not be renewed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := newIDP(t)
			store := storeIn(t)
			if tt.login {
				store = stored(t, i, tt.expiry, tt.change)
			}
			env := func(key string) (string, bool) { return tt.env, key == login.EnvToken && tt.env != "" }

			token, err := login.Source{Store: store, Env: env}.Token(t.Context())

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Token = %q, %v; want an error mentioning %q", token, err, tt.wantErr)
				}
			} else if err != nil || token != tt.want {
				t.Fatalf("Token = %q, %v; want %q", token, err, tt.want)
			}
			grants, _ := i.seen()
			if refreshed := len(grants) == 1 && grants[0] == "refresh_token"; refreshed != tt.wantRefresh {
				t.Errorf("issuer was asked %v, want a refresh: %t", grants, tt.wantRefresh)
			}
		})
	}
}

// A refresh is stored: the next command uses the new tokens, and the new
// refresh token replaces the spent one.
func TestRefreshIsStored(t *testing.T) {
	i := newIDP(t)
	now := time.Now()
	store := stored(t, i, now.Add(-time.Minute), nil)
	source := login.Source{Store: store, Env: noEnv, Now: func() time.Time { return now }}

	first, err := source.Token(t.Context())
	second, againErr := source.Token(t.Context())

	if err != nil || againErr != nil || first != "access-2" || second != "access-2" {
		t.Fatalf("Token = %q (%v), then %q (%v)", first, err, second, againErr)
	}
	if grants, _ := i.seen(); len(grants) != 1 {
		t.Errorf("issuer was asked %v, want one refresh", grants)
	}
	saved, err := store.Load()
	if err != nil || saved.AccessToken != "access-2" || saved.RefreshToken != "refresh-2" || saved.IDToken != "id-1" || !saved.Expiry.After(now) {
		t.Errorf("stored login = %+v, %v", saved, err)
	}
	if _, forms := i.seen(); forms[0].Get("client_id") != clientID || forms[0].Get("client_secret") != "" {
		t.Errorf("refresh request = %v, want the public client without a secret", forms[0])
	}
}

// A login file that cannot be used is an error, not a silent anonymous
// call.
func TestTokenSourceReportsABrokenStore(t *testing.T) {
	i := newIDP(t)
	i.refreshFails = true
	store := stored(t, i, time.Now().Add(-time.Hour), nil)

	_, err := login.Source{Store: store, Env: noEnv}.Token(t.Context())

	if err == nil || !strings.Contains(err.Error(), "could not be renewed") {
		t.Fatalf("Token = %v", err)
	}
}
