package login_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/cli/login"
)

func TestBrowserCallbackReportsVerifiedAuthentication(t *testing.T) {
	tests := []struct {
		name          string
		configure     func(*idp)
		wantError     string
		providerError string
	}{
		{name: "success"},
		{name: "exchange rejected", configure: func(i *idp) { i.exchangeError = "not_allowed" }, wantError: "offline_access"},
		{name: "unknown provider error", configure: func(i *idp) { i.exchangeError = "private-error-code" }, wantError: "request_failed"},
		{name: "invalid ID signature", configure: func(i *idp) { i.wrongSigner = true }, wantError: "invalid ID token"},
		{name: "invalid ID nonce", configure: func(i *idp) { i.changeClaims = func(c map[string]any) { c["nonce"] = "wrong" } }, wantError: "invalid nonce"},
		{name: "missing required ID token", configure: func(i *idp) { i.omitID = true }, wantError: "no required ID token"},
		{name: "provider refusal", providerError: "not_allowed", wantError: "offline_access"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := newIDP(t)
			if tt.configure != nil {
				tt.configure(i)
			}
			var page string
			var status int
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			creds, err := login.Login(ctx, login.Options{Issuer: i.URL, ClientID: clientID, Browser: true,
				OpenBrowser: func(target string) error {
					// Deliberately synchronous: Login must complete exchange and ID
					// verification without waiting for OpenBrowser to return.
					if tt.providerError != "" {
						u, err := url.Parse(target)
						if err != nil {
							return err
						}
						back, err := url.Parse(u.Query().Get("redirect_uri"))
						if err != nil {
							return err
						}
						back.RawQuery = url.Values{"state": {u.Query().Get("state")}, "error": {tt.providerError}, "error_description": {"private-provider-description code-1 access-1"}}.Encode()
						target = back.String()
					}
					res, err := http.Get(target)
					if err != nil {
						return err
					}
					defer func() { _ = res.Body.Close() }()
					status = res.StatusCode
					body, err := io.ReadAll(res.Body)
					page = string(body)
					return err
				}})
			if tt.wantError == "" {
				if err != nil || creds == nil {
					t.Fatalf("Login failed: %v", err)
				}
				if status != http.StatusOK || !strings.Contains(page, "Authentication completed") || !strings.Contains(page, "confirm your credentials were saved") {
					t.Fatalf("success page = %d %q", status, page)
				}
			} else {
				if err == nil || creds != nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Login error = %v, want %q", err, tt.wantError)
				}
				if status != http.StatusBadRequest || !strings.Contains(page, "Authentication failed") || !strings.Contains(page, tt.wantError) || strings.Contains(page, "Authentication completed") {
					t.Fatalf("failure page = %d %q", status, page)
				}
				for _, private := range []string{"private-provider-description", "private-error-code", "code-1", "access-1"} {
					if strings.Contains(page, private) || strings.Contains(err.Error(), private) {
						t.Errorf("callback exposed provider data %q", private)
					}
				}
			}
		})
	}
}

func TestBrowserCallbackCompletesResponseAfterLoginReturns(t *testing.T) {
	i := newIDP(t)
	response := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := login.Login(ctx, login.Options{Issuer: i.URL, ClientID: clientID, Browser: true,
		OpenBrowser: func(target string) error {
			go func() {
				res, err := http.Get(target)
				if err != nil {
					response <- err
					return
				}
				defer func() { _ = res.Body.Close() }()
				body, err := io.ReadAll(res.Body)
				if err == nil && (res.StatusCode != http.StatusOK || !strings.Contains(string(body), "Authentication completed")) {
					err = fmt.Errorf("incomplete callback response: status %d", res.StatusCode)
				}
				response <- err
			}()
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-response:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("browser response did not finish")
	}
}

func TestBrowserCallbackNoiseDoesNotCancelLogin(t *testing.T) {
	i := newIDP(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	creds, err := login.Login(ctx, login.Options{Issuer: i.URL, ClientID: clientID, Browser: true,
		OpenBrowser: func(target string) error {
			u, err := url.Parse(target)
			if err != nil {
				return err
			}
			back, err := url.Parse(u.Query().Get("redirect_uri"))
			if err != nil {
				return err
			}
			for _, states := range [][]string{nil, {"forged"}, {u.Query().Get("state"), "forged"}} {
				q := url.Values{"code": {"code-1"}}
				if states != nil {
					q["state"] = states
				}
				back.RawQuery = q.Encode()
				res, err := http.Get(back.String())
				if err != nil {
					return err
				}
				_ = res.Body.Close()
				if res.StatusCode != http.StatusBadRequest {
					return fmt.Errorf("noise callback status = %d", res.StatusCode)
				}
			}
			return visitLogin(target)
		}})
	if err != nil || creds == nil {
		t.Fatalf("legitimate login after noise failed: %v", err)
	}
	grants, _ := i.seen()
	if len(grants) != 1 || grants[0] != "authorization_code" {
		t.Fatalf("token exchanges = %v, want only legitimate callback", grants)
	}
}
