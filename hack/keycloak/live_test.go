package main

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/oauth2"

	"github.com/yindia/cluster-vending/pkg/auth"
)

// Opt-in integration test: exercises real browser-code PKCE and signed access
// tokens, using private local fixture credentials without printing them.
func TestLiveKeycloakPKCE(t *testing.T) {
	if os.Getenv("CV_KEYCLOAK_LIVE") != "1" {
		t.Skip("set CV_KEYCLOAK_LIVE=1 after hack/keycloak.sh start")
	}
	secrets, err := readSecrets(filepath.Join("..", ".keycloak", "secrets.env"))
	if err != nil || secrets == nil {
		t.Fatal("private fixture credentials unavailable")
	}
	cfg, err := localAuth().Resolved()
	if err != nil {
		t.Fatal(err)
	}
	verifier := auth.NewVerifier(cfg)
	for _, fixture := range []struct {
		user, passwordKey string
		role              auth.Role
	}{
		{"acme-admin", "KEYCLOAK_ACME_ADMIN_PASSWORD", auth.RoleTenantAdmin},
		{"acme-viewer", "KEYCLOAK_ACME_VIEWER_PASSWORD", auth.RoleTenantViewer},
	} {
		for _, clientID := range []string{"cv-cli", "cv-cli-confidential", "cv-ui"} {
			t.Run(fixture.user+"/"+clientID, func(t *testing.T) {
				jar, err := cookiejar.New(nil)
				if err != nil {
					t.Fatal(err)
				}
				client := &http.Client{Jar: jar, Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
					if req.URL.Port() == "18081" || req.URL.Port() == "3000" {
						return http.ErrUseLastResponse
					}
					if len(via) >= 10 {
						return io.ErrUnexpectedEOF
					}
					return nil
				}}
				ctx := context.WithValue(context.Background(), oauth2.HTTPClient, client)
				conf := oauth2.Config{ClientID: clientID, RedirectURL: "http://localhost:18081/callback", Scopes: []string{"openid", "profile", "email"}, Endpoint: oauth2.Endpoint{AuthURL: issuer + "/protocol/openid-connect/auth", TokenURL: issuer + "/protocol/openid-connect/token", AuthStyle: oauth2.AuthStyleInParams}}
				switch clientID {
				case "cv-cli-confidential":
					conf.ClientSecret = secrets["KEYCLOAK_CLI_CLIENT_SECRET"]
				case "cv-ui":
					conf.ClientSecret = secrets["KEYCLOAK_UI_CLIENT_SECRET"]
					conf.RedirectURL = "http://localhost:3000/api/auth/callback/keycloak"
				}
				proof := oauth2.GenerateVerifier()
				res, err := client.Get(conf.AuthCodeURL("local-smoke", oauth2.S256ChallengeOption(proof)))
				if err != nil {
					t.Fatal("authorization endpoint unavailable")
				}
				loginURL := loginForm(res.Body)
				res.Body.Close()
				if loginURL == "" {
					t.Fatal("login form not returned")
				}
				res, err = client.PostForm(loginURL, url.Values{"username": {fixture.user}, "password": {secrets[fixture.passwordKey]}, "credentialId": {""}})
				if err != nil {
					t.Fatal("login request failed")
				}
				res.Body.Close()
				back, err := url.Parse(res.Header.Get("Location"))
				if err != nil || back.Query().Get("code") == "" || back.Query().Get("state") != "local-smoke" {
					t.Fatal("login did not return an authorization code")
				}
				token, err := conf.Exchange(ctx, back.Query().Get("code"), oauth2.VerifierOption(proof))
				if err != nil {
					t.Fatal("PKCE exchange failed")
				}
				id, err := verifier.Verify(ctx, token.AccessToken)
				if err != nil {
					t.Fatal("API JWT verification failed")
				}
				if id.Username != fixture.user || id.IsPlatformAdmin() || len(id.Grants) != 1 || id.Grants[0].Role != fixture.role || id.Grants[0].Tenants[0] != "acme" {
					t.Fatal("unexpected tenant grants")
				}
				if err := id.Authorize(auth.Access{Scope: auth.ScopeTenant}, "globex"); err == nil {
					t.Fatal("cross-tenant access was granted")
				}
				if fixture.role == auth.RoleTenantViewer && id.Authorize(auth.Access{Scope: auth.ScopeTenant, Write: true}, "acme") == nil {
					t.Fatal("viewer write was granted")
				}
			})
		}
	}
}

func loginForm(body io.Reader) string {
	z := html.NewTokenizer(body)
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken:
			tag := z.Token()
			if tag.Data == "form" {
				for _, attr := range tag.Attr {
					if attr.Key == "action" {
						return attr.Val
					}
				}
			}
		}
	}
}
