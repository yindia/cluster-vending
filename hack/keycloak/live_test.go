package main

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/oauth2"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/cli/login"
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
	for _, u := range localUsers {
		fixture := struct{ user, passwordKey string }{u[0], u[1]}
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
				if clientID != "cv-ui" {
					// Exercise exactly the CLI defaults, including the scope that
					// requires the user's built-in offline_access realm role.
					conf.Scopes = []string{"openid", "offline_access"}
				}
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
				_ = res.Body.Close()
				if loginURL == "" {
					t.Fatal("login form not returned")
				}
				res, err = client.PostForm(loginURL, url.Values{"username": {fixture.user}, "password": {secrets[fixture.passwordKey]}, "credentialId": {""}})
				if err != nil {
					t.Fatal("login request failed")
				}
				_ = res.Body.Close()
				back, err := url.Parse(res.Header.Get("Location"))
				if err != nil || back.Query().Get("code") == "" || back.Query().Get("state") != "local-smoke" {
					t.Fatal("login did not return an authorization code")
				}
				token, err := conf.Exchange(ctx, back.Query().Get("code"), oauth2.VerifierOption(proof))
				if err != nil {
					t.Fatal("PKCE exchange failed")
				}
				if clientID != "cv-ui" {
					scope, _ := token.Extra("scope").(string)
					if token.RefreshToken == "" || !strings.Contains(" "+scope+" ", " offline_access ") {
						t.Fatal("CLI defaults did not receive offline refresh access")
					}
				}
				id, err := verifier.Verify(ctx, token.AccessToken)
				if err != nil {
					t.Fatal("API JWT verification failed")
				}
				if id.Anonymous || id.Username != fixture.user || id.Issuer != "keycloak" {
					t.Fatal("unexpected identity")
				}
			})
		}
	}
}

// Use the real CLI implementation and its default scopes, not a parallel
// hand-written token exchange, so callback-page and offline-access bugs regress.
func TestLiveKeycloakCLIBrowser(t *testing.T) {
	if os.Getenv("CV_KEYCLOAK_LIVE") != "1" {
		t.Skip("set CV_KEYCLOAK_LIVE=1 with the local callback port 18081 available")
	}
	secrets, err := readSecrets(filepath.Join("..", ".keycloak", "secrets.env"))
	if err != nil || secrets == nil {
		t.Fatal("private fixture credentials unavailable")
	}
	for _, clientID := range []string{"cv-cli", "cv-cli-confidential"} {
		t.Run(clientID, func(t *testing.T) {
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			browser := &http.Client{Jar: jar, Timeout: 15 * time.Second}
			open := func(target string) error {
				response, err := browser.Get(target)
				if err != nil {
					return io.ErrUnexpectedEOF
				}
				action := loginForm(response.Body)
				_ = response.Body.Close()
				if action == "" {
					return io.ErrUnexpectedEOF
				}
				response, err = browser.PostForm(action, url.Values{"username": {"acme-admin"}, "password": {secrets["KEYCLOAK_ACME_ADMIN_PASSWORD"]}, "credentialId": {""}})
				if err != nil {
					return io.ErrUnexpectedEOF
				}
				defer func() { _ = response.Body.Close() }()
				body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
				if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Authentication completed") {
					t.Error("browser did not report verified authentication")
				}
				return nil
			}
			options := login.Options{Issuer: issuer, ClientID: clientID, Browser: true, RedirectHost: "localhost", CallbackPort: 18081, OpenBrowser: open, Out: io.Discard}
			if clientID == "cv-cli-confidential" {
				options.ClientSecret = secrets["KEYCLOAK_CLI_CLIENT_SECRET"]
			}
			credentials, err := login.Login(t.Context(), options)
			if err != nil || credentials == nil || credentials.RefreshToken == "" {
				t.Fatal("real CLI browser login with default scopes failed")
			}
			cfg, err := localAuth().Resolved()
			if err != nil {
				t.Fatal(err)
			}
			identity, err := auth.NewVerifier(cfg).Verify(t.Context(), credentials.Bearer())
			if err != nil || identity.Username != "acme-admin" {
				t.Fatal("CLI token did not verify with the expected identity")
			}
		})
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
		default:
			// Only form start tags carry the login action.
		}
	}
}
