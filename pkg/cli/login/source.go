package login

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// EnvToken is the environment variable whose value is sent as the bearer
// token, whatever is stored: for automation that gets its token elsewhere
// (Microsoft Entra ID: the client credentials flow).
const EnvToken = "CV_TOKEN"

var refreshLocks sync.Map

const (
	// expirySlack refreshes a token this long before it expires: a request
	// must not arrive with one that expires on the way.
	expirySlack = 30 * time.Second
	// requestTimeout bounds one call to the issuer.
	requestTimeout = 30 * time.Second
)

// Source hands out the bearer token the API is called with.
type Source struct {
	// Store holds the login.
	Store Store
	// Env reads the environment; nil: os.LookupEnv.
	Env func(string) (string, bool)
	// HTTPClient calls the issuer's token endpoint; nil: a client with
	// requestTimeout.
	HTTPClient *http.Client
	// Now is the clock; nil: time.Now.
	Now func() time.Time
}

// Token is the bearer token to send: CV_TOKEN when set, else the stored
// login's, refreshed first when it has expired. "" without an error when
// nobody is logged in: the call is made without a token, and a server
// that needs one says so.
func (s Source) Token(ctx context.Context) (string, error) {
	env := s.Env
	if env == nil {
		env = os.LookupEnv
	}
	if token, ok := env(EnvToken); ok && token != "" {
		return token, nil
	}
	lock, _ := refreshLocks.LoadOrStore(s.Store.Path, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	creds, err := s.Store.Load()
	if err != nil || creds == nil {
		return "", err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if creds.Expiry.IsZero() || now().Add(expirySlack).Before(creds.Expiry) {
		return creds.Bearer(), nil
	}
	if creds.RefreshToken == "" {
		return "", errors.New("your login has expired: run `cluster-vending login` again")
	}
	if err := s.refresh(ctx, creds); err != nil {
		return "", err
	}
	return creds.Bearer(), nil
}

// refresh trades the refresh token for new tokens and stores them.
func (s Source) refresh(ctx context.Context, creds *Credentials) error {
	if !safeEndpoint(creds.TokenURL) || !safeEndpoint(creds.Issuer) {
		return errors.New("stored issuer/token endpoint is unsafe; log in again")
	}
	conf := oauth2.Config{ClientID: creds.ClientID, ClientSecret: creds.ClientSecret, Scopes: creds.Scopes,
		Endpoint: oauth2.Endpoint{TokenURL: creds.TokenURL, AuthStyle: clientAuthStyle(creds.ClientSecret)}}
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, oauth2.HTTPClient, s.client()), requestTimeout)
	defer cancel()
	token, err := conf.TokenSource(ctx, &oauth2.Token{RefreshToken: creds.RefreshToken}).Token()
	if err != nil {
		return fmt.Errorf("your login has expired and could not be renewed (%w): run `cluster-vending login` again", safeOAuthError(err))
	}
	rawID, _ := token.Extra("id_token").(string)
	if rawID != "" || creds.UseIDToken {
		provider, err := oidc.NewProvider(oidc.ClientContext(ctx, s.client()), creds.Issuer)
		if err != nil {
			return errors.New("could not verify renewed ID token; log in again")
		}
		id, err := verifyLoginIDToken(ctx, provider, creds.ClientID, token, creds.Nonce, creds.UseIDToken, false)
		if err != nil {
			return err
		}
		if id != nil && creds.Subject != "" && id.Subject != creds.Subject {
			return errors.New("renewed ID token changed subject; log in again")
		}
		if id != nil && creds.UseIDToken && (token.Expiry.IsZero() || id.Expiry.Before(token.Expiry)) {
			token.Expiry = id.Expiry
		}
	}
	creds.apply(token)
	return s.Store.Save(creds)
}

func (s Source) client() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: requestTimeout}
}

// apply takes the tokens of an issuer's answer. An answer without a new
// refresh or ID token keeps the old one.
func (c *Credentials) apply(token *oauth2.Token) {
	c.AccessToken, c.Expiry = token.AccessToken, token.Expiry
	if token.RefreshToken != "" {
		c.RefreshToken = token.RefreshToken
	}
	if id, ok := token.Extra("id_token").(string); ok && id != "" {
		c.IDToken = id
	}
}
