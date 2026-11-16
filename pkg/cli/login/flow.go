package login

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/yindia/cluster-vending/pkg/auth"
)

// Scopes asked for when none are given: an ID token, and a refresh token
// so the login outlives the access token.
var defaultScopes = []string{oidc.ScopeOpenID, oidc.ScopeOfflineAccess}

var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const (
	// callbackPath is where the issuer sends the browser back to.
	callbackPath = "/callback"
	// defaultRedirectHost is the loopback name in the redirect URI: what
	// Microsoft Entra ID and most issuers register for native clients.
	defaultRedirectHost = "localhost"
)

// Options are what a login needs.
type Options struct {
	// Issuer is the OIDC issuer URL; ClientID is the CLI's registered client.
	Issuer   string
	ClientID string
	// ClientSecret is optional confidential-client authentication, sent only to
	// the token endpoint. CLI callers obtain it from a private source, not argv.
	ClientSecret string
	// Scopes to ask for; empty: openid and offline_access.
	Scopes []string

	// EntraTenant, EntraClientID and EntraScope are the Microsoft Entra ID
	// preset, in place of Issuer, ClientID and Scopes: the directory
	// (tenant) ID, the CLI's app registration, and the API's scope, e.g.
	// api://<app-id>/.default.
	EntraTenant   string
	EntraClientID string
	EntraScope    string

	// Browser logs in through the browser on this machine (authorization
	// code with PKCE); otherwise the device authorization grant is used,
	// which works over SSH.
	Browser bool
	// UseIDToken sends the ID token to the API (Credentials.UseIDToken).
	UseIDToken bool

	// Out receives the instructions for the user.
	Out io.Writer
	// HTTPClient talks to the issuer; nil: a client with requestTimeout.
	HTTPClient *http.Client
	// OpenBrowser opens a URL in the user's browser; nil: the URL is only
	// printed.
	OpenBrowser func(url string) error
	// RedirectHost is the host of the browser flow's redirect URI; empty:
	// localhost.
	RedirectHost string
	// CallbackPort is the loopback listener port; zero selects an ephemeral port.
	CallbackPort int
	nonce        string
}

// resolved applies the Entra preset and the defaults, and reports what is
// missing.
func (o Options) resolved() (Options, error) {
	entra := o.EntraTenant != "" || o.EntraClientID != "" || o.EntraScope != ""
	switch {
	case entra && (o.Issuer != "" || o.ClientID != ""):
		return o, errors.New("give either --issuer and --client-id, or the --entra-* flags, not both")
	case entra:
		if !guidPattern.MatchString(o.EntraTenant) || o.EntraClientID == "" || o.EntraScope == "" {
			return o, errors.New("--entra-tenant (the directory ID, a GUID), --entra-client-id and --entra-scope (e.g. api://<app-id>/.default) are all required")
		}
		o.Issuer, o.ClientID = auth.EntraIssuer(strings.ToLower(o.EntraTenant)), o.EntraClientID
		if len(o.Scopes) == 0 {
			// The API's scope makes the access token one for the API;
			// offline_access adds the refresh token.
			o.Scopes = []string{o.EntraScope, oidc.ScopeOfflineAccess}
		}
	case o.Issuer == "" || o.ClientID == "":
		return o, errors.New("--issuer and --client-id are required (or the --entra-* flags)")
	}
	if len(o.Scopes) == 0 {
		o.Scopes = slices.Clone(defaultScopes)
	}
	if o.UseIDToken && !slices.Contains(o.Scopes, oidc.ScopeOpenID) {
		return o, errors.New("--use-id-token needs the openid scope")
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: requestTimeout}
	}
	if o.RedirectHost == "" {
		o.RedirectHost = defaultRedirectHost
	}
	if o.RedirectHost != "localhost" && o.RedirectHost != "127.0.0.1" && o.RedirectHost != "::1" {
		return o, errors.New("--redirect-host must be localhost, 127.0.0.1 or ::1")
	}
	if o.CallbackPort < 0 || o.CallbackPort > 65535 {
		return o, errors.New("--callback-port must be between 0 and 65535")
	}
	if o.ClientSecret != "" && !o.Browser {
		return o, errors.New("confidential clients require --browser; device-code login uses a public client")
	}
	if !safeEndpoint(o.Issuer) {
		return o, errors.New("issuer must use HTTPS, or HTTP on a loopback host, without URL credentials")
	}
	return o, nil
}

// Login logs the user in at the issuer and returns the credentials; the
// caller stores them (Store.Save). It blocks until the user finished in
// the browser, the issuer refused, or ctx ended.
func Login(ctx context.Context, opts Options) (*Credentials, error) {
	o, err := opts.resolved()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	ctx = oidc.ClientContext(ctx, o.HTTPClient)
	provider, err := oidc.NewProvider(ctx, o.Issuer)
	if err != nil {
		return nil, fmt.Errorf("read the issuer's configuration: %w", err)
	}
	conf := &oauth2.Config{ClientID: o.ClientID, ClientSecret: o.ClientSecret, Scopes: o.Scopes, Endpoint: provider.Endpoint()}
	for _, endpoint := range []string{conf.Endpoint.AuthURL, conf.Endpoint.TokenURL, conf.Endpoint.DeviceAuthURL} {
		if endpoint != "" && !safeEndpoint(endpoint) {
			return nil, errors.New("issuer advertised an unsafe OAuth endpoint")
		}
	}
	conf.Endpoint.AuthStyle = clientAuthStyle(o.ClientSecret)
	if o.Browser {
		o.nonce, err = randomState()
		if err != nil {
			return nil, err
		}
	}
	validate := func(token *oauth2.Token) (*oidc.IDToken, error) {
		id, err := verifyLoginIDToken(ctx, provider, o.ClientID, token, o.nonce, slices.Contains(o.Scopes, oidc.ScopeOpenID) || o.UseIDToken, true)
		if err != nil {
			return nil, err
		}
		if !o.UseIDToken && token.AccessToken == "" {
			return nil, errors.New("issuer returned no access token")
		}
		return id, nil
	}
	var token *oauth2.Token
	var id *oidc.IDToken
	if o.Browser {
		token, id, err = o.browser(ctx, conf, validate)
	} else {
		token, err = o.device(ctx, conf)
		if err == nil {
			id, err = validate(token)
		}
	}
	if err != nil {
		return nil, err
	}
	creds := &Credentials{Issuer: o.Issuer, ClientID: o.ClientID, ClientSecret: o.ClientSecret, TokenURL: conf.Endpoint.TokenURL,
		Scopes: o.Scopes, UseIDToken: o.UseIDToken, Nonce: o.nonce}
	if id != nil {
		creds.Subject = id.Subject
	}
	creds.apply(token)
	if id != nil && o.UseIDToken && (creds.Expiry.IsZero() || id.Expiry.Before(creds.Expiry)) {
		creds.Expiry = id.Expiry
	}
	if creds.Bearer() == "" {
		return nil, errors.New("the issuer returned no token to call the API with (with --use-id-token: no ID token)")
	}
	return creds, nil
}

// device is the device authorization grant (RFC 8628): the user opens a
// URL on any device and enters a code, while this side polls.
func (o Options) device(ctx context.Context, conf *oauth2.Config) (*oauth2.Token, error) {
	if conf.Endpoint.DeviceAuthURL == "" {
		return nil, errors.New("the issuer has no device authorization endpoint: log in with --browser")
	}
	grant, err := conf.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("start the login: %w", safeOAuthError(err))
	}
	if grant.VerificationURIComplete != "" {
		_, _ = fmt.Fprintf(o.Out, "To log in, open %s\n(or %s and enter the code %s)\n",
			grant.VerificationURIComplete, grant.VerificationURI, grant.UserCode)
	} else {
		_, _ = fmt.Fprintf(o.Out, "To log in, open %s and enter the code %s\n", grant.VerificationURI, grant.UserCode)
	}
	_, _ = fmt.Fprintln(o.Out, "Waiting for you to finish in the browser...")
	token, err := conf.DeviceAccessToken(ctx, grant)
	if err != nil {
		return nil, fmt.Errorf("login not completed: %w", safeOAuthError(err))
	}
	return token, nil
}

// callback is the completed exchange/verification result, never a raw code.
type callback struct {
	token *oauth2.Token
	id    *oidc.IDToken
	err   error
}

// browser is the authorization code grant with PKCE (RFC 7636) and a
// redirect to a loopback port (RFC 8252): the user logs in in the browser
// of this machine.
func (o Options) browser(ctx context.Context, conf *oauth2.Config, validate func(*oauth2.Token) (*oidc.IDToken, error)) (*oauth2.Token, *oidc.IDToken, error) {
	listenHost := "127.0.0.1"
	if o.RedirectHost == "::1" {
		listenHost = "::1"
	}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", net.JoinHostPort(listenHost, fmt.Sprint(o.CallbackPort)))
	if err != nil {
		return nil, nil, fmt.Errorf("listen for the browser's return: %w", err)
	}
	state, err := randomState()
	if err != nil {
		_ = listener.Close()
		return nil, nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	conf.RedirectURL = fmt.Sprintf("http://%s%s", net.JoinHostPort(o.RedirectHost, fmt.Sprint(port)), callbackPath)
	verifier := oauth2.GenerateVerifier()
	results := make(chan callback, 1)
	finish := func(code string) callback {
		token, err := conf.Exchange(ctx, code, oauth2.VerifierOption(verifier))
		if err != nil {
			return callback{err: safeOAuthError(err)}
		}
		id, err := validate(token)
		return callback{token: token, id: id, err: err}
	}
	// Finish on the HTTP handler goroutine, independently of OpenBrowser: a
	// synchronous browser/test client may wait for this response before returning.
	server := &http.Server{Handler: callbackHandler(state, finish, results), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }() // closes the listener when it stops
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
		}
	}()

	authURL := conf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(o.nonce))
	_, _ = fmt.Fprintf(o.Out, "To log in, open this URL in your browser:\n%s\n", authURL)
	if o.OpenBrowser != nil {
		if err := o.OpenBrowser(authURL); err != nil {
			_, _ = fmt.Fprintln(o.Out, "(could not open the browser; open the URL above manually)")
		}
	}
	select {
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("login not completed: %w", ctx.Err())
	case result := <-results:
		if result.err != nil {
			return nil, nil, fmt.Errorf("login not completed: %w", result.err)
		}
		return result.token, result.id, nil
	}
}

// callbackHandler accepts one state-matched response, and reports authentication
// success only after exchange and verification. Unrelated loopback noise cannot
// claim the callback or terminate the legitimate login.
func callbackHandler(state string, finish func(string) callback, results chan<- callback) http.Handler {
	mux := http.NewServeMux()
	var claimed atomic.Bool
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		q := r.URL.Query()
		if len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "This callback does not match the active login. Return to the original login page.", http.StatusBadRequest)
			return
		}
		if !claimed.CompareAndSwap(false, true) {
			http.Error(w, "This login callback has already been received. Check the terminal for its result.", http.StatusConflict)
			return
		}
		var result callback
		switch {
		case q.Get("error") != "":
			result.err = fmt.Errorf("the issuer refused: %s", safeOAuthMessage(q.Get("error")))
		case len(q["code"]) > 1:
			result.err = errors.New("the browser came back with duplicate codes")
		case q.Get("code") == "":
			result.err = errors.New("the browser came back without a code")
		default:
			result = finish(q.Get("code"))
		}
		if result.err != nil {
			http.Error(w, "Authentication failed: "+result.err.Error()+". Return to the terminal for the login result.", http.StatusBadRequest)
		} else {
			_, _ = fmt.Fprintln(w, "Authentication completed. Return to the terminal to confirm your credentials were saved. You can close this window.")
		}
		// Ensure the page reaches the browser before publishing the result:
		// browser() closes the listener when it receives this buffered message.
		_ = http.NewResponseController(w).Flush()
		select {
		case results <- result:
		default: // a second request: the first one decided
		}
	})
	return mux
}

func safeEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
}

func clientAuthStyle(secret string) oauth2.AuthStyle {
	if secret != "" {
		return oauth2.AuthStyleInHeader
	}
	return oauth2.AuthStyleInParams
}

func safeOAuthCode(code string) string {
	switch code {
	case "invalid_request", "invalid_client", "invalid_grant", "unauthorized_client", "unsupported_grant_type", "invalid_scope", "access_denied", "server_error", "temporarily_unavailable", "expired_token", "not_allowed":
		return code
	default:
		return "request_failed"
	}
}

func safeOAuthMessage(code string) string {
	code = safeOAuthCode(code)
	if code == "not_allowed" {
		return code + ": issuer policy denied the request; if offline_access was requested, check the client's and user's offline-access permissions"
	}
	return code
}

// OAuth response bodies/descriptions can echo a credential. Never surface them.
func safeOAuthError(err error) error {
	var response *oauth2.RetrieveError
	if errors.As(err, &response) {
		return errors.New(safeOAuthMessage(response.ErrorCode))
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New("issuer request failed")
}

func verifyLoginIDToken(ctx context.Context, provider *oidc.Provider, clientID string, token *oauth2.Token, nonce string, required, requireNonce bool) (*oidc.IDToken, error) {
	raw, _ := token.Extra("id_token").(string)
	if raw == "" {
		if required {
			return nil, errors.New("issuer returned no required ID token")
		}
		return nil, nil
	}
	id, err := provider.Verifier(&oidc.Config{ClientID: clientID, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}}).Verify(ctx, raw)
	if err != nil {
		return nil, errors.New("issuer returned an invalid ID token")
	}
	if id.Subject == "" {
		return nil, errors.New("issuer returned an ID token without a subject")
	}
	if nonce != "" && (requireNonce || id.Nonce != "") && subtle.ConstantTimeCompare([]byte(nonce), []byte(id.Nonce)) != 1 {
		return nil, errors.New("issuer returned an ID token with an invalid nonce")
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
	}
	if err := id.Claims(&claims); err != nil || (claims.AuthorizedParty != "" && claims.AuthorizedParty != clientID) || (len(id.Audience) > 1 && claims.AuthorizedParty != clientID) {
		return nil, errors.New("issuer returned an ID token with an invalid authorized party")
	}
	if id.AccessTokenHash != "" && id.VerifyAccessToken(token.AccessToken) != nil {
		return nil, errors.New("issuer returned an ID token with an invalid access token hash")
	}
	return id, nil
}

// randomState is an unguessable value tying the browser's return to this
// login.
func randomState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("random state: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
