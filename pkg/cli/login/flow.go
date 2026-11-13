package login

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
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
	// Issuer is the OIDC issuer URL; ClientID the public client the CLI
	// logs in as (no secret).
	Issuer   string
	ClientID string
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
	ctx = oidc.ClientContext(ctx, o.HTTPClient)
	provider, err := oidc.NewProvider(ctx, o.Issuer)
	if err != nil {
		return nil, fmt.Errorf("read the issuer's configuration: %w", err)
	}
	conf := &oauth2.Config{ClientID: o.ClientID, Scopes: o.Scopes, Endpoint: provider.Endpoint()}
	conf.Endpoint.AuthStyle = oauth2.AuthStyleInParams // a public client: no secret to send
	flow := o.device
	if o.Browser {
		flow = o.browser
	}
	token, err := flow(ctx, conf)
	if err != nil {
		return nil, err
	}
	creds := &Credentials{Issuer: o.Issuer, ClientID: o.ClientID, TokenURL: conf.Endpoint.TokenURL,
		Scopes: o.Scopes, UseIDToken: o.UseIDToken}
	creds.apply(token)
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
		return nil, fmt.Errorf("start the login: %w", err)
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
		return nil, fmt.Errorf("login not completed: %w", err)
	}
	return token, nil
}

// callback is what the issuer sent the browser back with.
type callback struct {
	code string
	err  error
}

// browser is the authorization code grant with PKCE (RFC 7636) and a
// redirect to a loopback port (RFC 8252): the user logs in in the browser
// of this machine.
func (o Options) browser(ctx context.Context, conf *oauth2.Config) (*oauth2.Token, error) {
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for the browser's return: %w", err)
	}
	state, err := randomState()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	results := make(chan callback, 1)
	server := &http.Server{Handler: callbackHandler(state, results), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }() // closes the listener when it stops
	defer func() { _ = server.Close() }()

	port := listener.Addr().(*net.TCPAddr).Port
	conf.RedirectURL = fmt.Sprintf("http://%s%s", net.JoinHostPort(o.RedirectHost, fmt.Sprint(port)), callbackPath)
	verifier := oauth2.GenerateVerifier()
	authURL := conf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
	_, _ = fmt.Fprintf(o.Out, "To log in, open this URL in your browser:\n%s\n", authURL)
	if o.OpenBrowser != nil {
		if err := o.OpenBrowser(authURL); err != nil {
			_, _ = fmt.Fprintf(o.Out, "(could not open the browser: %v)\n", err)
		}
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("login not completed: %w", ctx.Err())
	case result := <-results:
		if result.err != nil {
			return nil, fmt.Errorf("login not completed: %w", result.err)
		}
		token, err := conf.Exchange(ctx, result.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("login not completed: %w", err)
		}
		return token, nil
	}
}

// callbackHandler serves the redirect URI once: it checks the state and
// passes the code, or the issuer's error, on.
func callbackHandler(state string, results chan<- callback) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		result := callback{code: q.Get("code")}
		switch {
		case q.Get("state") != state:
			result.err = errors.New("the browser came back with another login's state")
		case q.Get("error") != "":
			result.err = fmt.Errorf("the issuer refused: %s %s", q.Get("error"), q.Get("error_description"))
		case result.code == "":
			result.err = errors.New("the browser came back without a code")
		}
		if result.err != nil {
			http.Error(w, "Login failed: "+result.err.Error()+". You can close this window.", http.StatusBadRequest)
		} else {
			_, _ = fmt.Fprintln(w, "Logged in. You can close this window and return to the terminal.")
		}
		select {
		case results <- result:
		default: // a second request: the first one decided
		}
	})
	return mux
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
