package login_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/cli/login"
)

func visitLogin(target string) error {
	response, err := http.Get(target)
	if err != nil {
		return err
	}
	return response.Body.Close()
}

func TestConfidentialBrowserLoginAndRefresh(t *testing.T) {
	i := newIDP(t)
	const secret = "private:quoted+value"
	i.clientSecret = secret
	var out bytes.Buffer
	var opened string
	creds, err := login.Login(t.Context(), login.Options{Issuer: i.URL, ClientID: clientID, ClientSecret: secret, Browser: true, RedirectHost: "127.0.0.1", Out: &out, OpenBrowser: func(target string) error { opened = target; return visitLogin(target) }})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(opened, secret) || strings.Contains(out.String(), secret) || strings.Contains(opened, "client_secret") {
		t.Fatal("client secret leaked into browser instructions")
	}
	u, _ := url.Parse(opened)
	if u.Query().Get("nonce") == "" || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("confidential client lost nonce or PKCE")
	}
	if creds.ClientSecret != secret || creds.Subject != "cli-user" {
		t.Fatal("private refresh credentials or verified subject missing")
	}
	creds.Expiry = time.Now().Add(-time.Minute)
	store := storeIn(t)
	if err := store.Save(creds); err != nil {
		t.Fatal(err)
	}
	token, err := (login.Source{Store: store, Env: noEnv}).Token(t.Context())
	if err != nil || token != "access-2" {
		t.Fatalf("refresh failed: %v", err)
	}
	i.mu.Lock()
	user, password := i.basicUser, i.basicSecret
	i.mu.Unlock()
	if user != clientID || password != secret {
		t.Fatal("token refresh omitted confidential client authentication")
	}
	_, forms := i.seen()
	for _, form := range forms {
		if form.Get("client_secret") != "" {
			t.Fatal("secret unexpectedly copied into request parameters")
		}
	}
	saved, err := store.Load()
	if err != nil || saved.ClientSecret != secret || saved.RefreshToken != "refresh-2" {
		t.Fatal("rotated credentials not saved privately")
	}
}

func TestConfidentialErrorsDoNotExposeSecret(t *testing.T) {
	i := newIDP(t)
	i.clientSecret = "expected-secret-do-not-echo"
	var out bytes.Buffer
	_, err := login.Login(t.Context(), login.Options{Issuer: i.URL, ClientID: clientID, ClientSecret: "wrong-secret", Browser: true, Out: &out, OpenBrowser: visitLogin})
	if err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("expected a sanitized invalid_client error, got %v", err)
	}
	if strings.Contains(err.Error(), i.clientSecret) || strings.Contains(err.Error(), "wrong-secret") || strings.Contains(out.String(), "wrong-secret") {
		t.Fatal("issuer response echoed a secret")
	}
}

func TestBrowserRejectsInvalidIDTokens(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*idp)
		want      string
	}{
		{"nonce", func(i *idp) { i.changeClaims = func(c map[string]any) { c["nonce"] = "other-login" } }, "nonce"},
		{"missing nonce", func(i *idp) { i.changeClaims = func(c map[string]any) { delete(c, "nonce") } }, "nonce"},
		{"issuer", func(i *idp) { i.changeClaims = func(c map[string]any) { c["iss"] = "https://other.example" } }, "invalid ID token"},
		{"audience", func(i *idp) { i.changeClaims = func(c map[string]any) { c["aud"] = "cv-api" } }, "invalid ID token"},
		{"expiry", func(i *idp) { i.changeClaims = func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() } }, "invalid ID token"},
		{"signature", func(i *idp) { i.wrongSigner = true }, "invalid ID token"},
		{"authorized party", func(i *idp) { i.changeClaims = func(c map[string]any) { c["azp"] = "other-client" } }, "authorized party"},
		{"missing ID token", func(i *idp) { i.omitID = true }, "no required ID token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			i := newIDP(t)
			test.configure(i)
			_, err := login.Login(t.Context(), login.Options{Issuer: i.URL, ClientID: clientID, Browser: true, OpenBrowser: visitLogin})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %s rejection, got %v", test.name, err)
			}
		})
	}
}

func TestBrowserExplicitLoopbackCallback(t *testing.T) {
	i := newIDP(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	var redirect string
	_, err = login.Login(t.Context(), login.Options{Issuer: i.URL, ClientID: clientID, Browser: true, RedirectHost: "127.0.0.1", CallbackPort: port, OpenBrowser: func(target string) error {
		u, _ := url.Parse(target)
		redirect = u.Query().Get("redirect_uri")
		return visitLogin(target)
	}})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(redirect)
	if u.Host != net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) {
		t.Fatal("configured callback port was not used")
	}
	if u.Hostname() != "127.0.0.1" || u.Path != "/callback" || u.Port() == "0" {
		t.Fatal("incorrect loopback callback")
	}
}

func TestLoginRejectsUnsafeOptions(t *testing.T) {
	for _, opts := range []login.Options{
		{Issuer: "http://remote.example", ClientID: clientID},
		{Issuer: "https://issuer.example", ClientID: clientID, RedirectHost: "remote.example", Browser: true},
		{Issuer: "https://issuer.example", ClientID: clientID, CallbackPort: 65536, Browser: true},
		{Issuer: "https://issuer.example", ClientID: clientID, ClientSecret: "secret"},
	} {
		if _, err := login.Login(t.Context(), opts); err == nil {
			t.Fatal("accepted unsafe login options")
		}
	}
}

func TestSecretSources(t *testing.T) {
	t.Setenv("TEST_CV_CLIENT_SECRET", "private-env-value")
	got, err := login.ReadClientSecret("TEST_CV_CLIENT_SECRET", "", nil)
	if err != nil || got != "private-env-value" {
		t.Fatal("environment secret not read")
	}
	file := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(file, []byte("private-file-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = login.ReadClientSecret("", file, nil)
	if err != nil || got != "private-file-value" {
		t.Fatal("private file secret not read")
	}
	got, err = login.ReadClientSecret("", "", func() (string, error) { return "tty-value", nil })
	if err != nil || got != "tty-value" {
		t.Fatal("terminal source not read")
	}
	if _, err := login.ReadClientSecret("TEST_CV_CLIENT_SECRET", file, nil); err == nil {
		t.Fatal("ambiguous secret sources accepted")
	}
	if _, err := login.ReadClientSecret("", "", func() (string, error) { return "", errors.New("echoed-sensitive-value") }); err == nil || strings.Contains(err.Error(), "echoed-sensitive-value") {
		t.Fatal("prompt error not sanitized")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(file, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := login.ReadClientSecret("", file, nil); err == nil {
			t.Fatal("public secret file accepted")
		}
		link := filepath.Join(t.TempDir(), "symlink")
		if err := os.Symlink(file, link); err != nil {
			t.Fatal(err)
		}
		if _, err := login.ReadClientSecret("", link, nil); err == nil {
			t.Fatal("symlink secret file accepted")
		}
	}
}

func TestStoreProtectsExistingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}
	store := storeIn(t)
	if err := os.MkdirAll(filepath.Dir(store.Path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&login.Credentials{AccessToken: "token", ClientSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Dir(store.Path))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("existing login directory remains public")
	}
	if err := os.Chmod(filepath.Dir(store.Path), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("credentials loaded from public directory")
	}
}

func TestRefreshSerializesRotatingCredentials(t *testing.T) {
	i := newIDP(t)
	store := stored(t, i, time.Now().Add(-time.Minute), nil)
	source := login.Source{Store: store, Env: noEnv}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			token, err := source.Token(context.Background())
			if err != nil || token != "access-2" {
				t.Errorf("concurrent refresh: %v", err)
			}
		})
	}
	wg.Wait()
	grants, _ := i.seen()
	if len(grants) != 1 {
		t.Fatalf("rotation sent %d refresh requests", len(grants))
	}
}

func TestRefreshValidatesReturnedIDToken(t *testing.T) {
	for _, changeSubject := range []bool{false, true} {
		t.Run(strconv.FormatBool(changeSubject), func(t *testing.T) {
			i := newIDP(t)
			creds, err := login.Login(t.Context(), login.Options{Issuer: i.URL, ClientID: clientID, Browser: true, UseIDToken: true, OpenBrowser: visitLogin})
			if err != nil {
				t.Fatal(err)
			}
			i.mu.Lock()
			i.refreshID = true
			i.changeClaims = func(claims map[string]any) {
				delete(claims, "nonce")
				if changeSubject {
					claims["sub"] = "different-user"
				}
			}
			i.mu.Unlock()
			creds.Expiry = time.Now().Add(-time.Minute)
			store := storeIn(t)
			if err := store.Save(creds); err != nil {
				t.Fatal(err)
			}
			token, err := (login.Source{Store: store, Env: noEnv}).Token(t.Context())
			if changeSubject {
				if err == nil || !strings.Contains(err.Error(), "changed subject") {
					t.Fatalf("subject switch was not rejected: %v", err)
				}
				saved, loadErr := store.Load()
				if loadErr != nil || saved.AccessToken != creds.AccessToken {
					t.Fatal("rejected refresh overwrote the stored login")
				}
			} else if err != nil || token != i.issuedID() {
				t.Fatalf("valid refreshed ID token without optional nonce was rejected: %v", err)
			}
		})
	}
}
