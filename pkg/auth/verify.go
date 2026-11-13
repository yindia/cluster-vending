package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/yindia/cluster-vending/pkg/fault"
)

const (
	// discoveryTimeout bounds reading an issuer's discovery document and
	// each fetch of its keys.
	discoveryTimeout = 10 * time.Second
	// discoveryRetry is how long a failed discovery is remembered: requests
	// in between fail at once, without asking the issuer again.
	discoveryRetry = 15 * time.Second
	// clockLeeway is how far past its expiry a token is still accepted (the
	// clocks of issuer and server differ). go-oidc gives "not before" five
	// minutes on its own.
	clockLeeway = 30 * time.Second
	// maxTokenBytes bounds a bearer token: nothing an issuer hands out
	// comes close, and a longer one is not worth parsing.
	maxTokenBytes = 16 << 10
)

// signingAlgorithms are the only signatures accepted, whatever the issuer
// advertises: never "none", never a shared secret (HS*).
var signingAlgorithms = []string{oidc.RS256, oidc.ES256}

// Verifier checks bearer tokens against the configured issuers and turns
// an accepted one into the caller it stands for. It reads an issuer's
// discovery document and keys on the first token that names it, not at
// startup: the server starts while an issuer is unreachable, and tokens of
// that issuer are refused until it is back.
type Verifier struct {
	rules   []Rule
	sources map[string]*source // by "iss"
	client  *http.Client
	now     func() time.Time
}

// VerifierOption adjusts a Verifier.
type VerifierOption func(*Verifier)

// WithHTTPClient sets the client that reads discovery documents and keys.
func WithHTTPClient(c *http.Client) VerifierOption { return func(v *Verifier) { v.client = c } }

// WithClock sets the clock tokens are checked against.
func WithClock(now func() time.Time) VerifierOption { return func(v *Verifier) { v.now = now } }

// NewVerifier is a Verifier of a resolved configuration (Config.Resolved).
func NewVerifier(cfg Config, opts ...VerifierOption) *Verifier {
	v := &Verifier{rules: slices.Clone(cfg.Rules), sources: map[string]*source{},
		client: &http.Client{Timeout: discoveryTimeout}, now: time.Now}
	for _, opt := range opts {
		opt(v)
	}
	for _, issuer := range cfg.Issuers {
		v.sources[issuer.Issuer] = &source{issuer: issuer, url: issuer.Issuer}
		for _, alt := range issuer.Alternates {
			v.sources[alt.Issuer] = &source{issuer: issuer, url: alt.Issuer, discovery: alt.DiscoveryURL}
		}
	}
	return v
}

// source is one "iss" value and the keys that verify its tokens.
type source struct {
	issuer Issuer
	// url is the "iss" value; discovery the base of its discovery document
	// when that is not url itself.
	url, discovery string

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
	failedAt time.Time
	failure  error
}

// Verify checks a bearer token: issued by a configured issuer, signed with
// one of its keys, for one of its audiences, and not expired. It returns
// the caller, or a fault: Unauthenticated for a token that is not
// accepted, Unavailable while its issuer cannot be reached. The fault's
// message is for the caller; the error's text adds the reason, for logs.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Identity, error) {
	iss, err := unverifiedIssuer(raw)
	if err != nil {
		return nil, rejected("the token is not a signed JWT", err)
	}
	src, ok := v.sources[iss]
	if !ok {
		return nil, rejected("the token's issuer is not accepted here", fmt.Errorf("issuer %q", iss))
	}
	verifier, err := src.get(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", fault.Newf(fault.Unavailable,
			"the identity provider %s cannot be reached to verify the token; try again", src.issuer.Name), err)
	}
	token, err := verifier.Verify(ctx, raw)
	var expired *oidc.TokenExpiredError
	switch {
	case errors.As(err, &expired):
		return nil, rejected("the token has expired; log in again", err)
	case err != nil:
		return nil, rejected("the token is not valid", err)
	}
	if !slices.ContainsFunc(token.Audience, func(a string) bool { return slices.Contains(src.issuer.Audiences, a) }) {
		return nil, rejected("the token was not issued for this API", fmt.Errorf("audience %q", token.Audience))
	}
	var claims map[string]any
	if err := token.Claims(&claims); err != nil {
		return nil, rejected("the token is not valid", err)
	}
	return v.identity(src.issuer, token.Subject, claims), nil
}

// identity is the caller a verified token of issuer stands for.
func (v *Verifier) identity(issuer Issuer, subject string, claims map[string]any) *Identity {
	id := &Identity{Subject: subject, Issuer: issuer.Name, Roles: claimStrings(claims[issuer.RolesClaim])}
	if id.Username = claimString(claims[issuer.UsernameClaim]); id.Username == "" {
		id.Username = subject
	}
	tenant := ""
	if issuer.TenantClaim != "" {
		tenant = claimString(claims[issuer.TenantClaim])
	}
	id.Grants = grantsFor(v.rules, issuer, id.Roles, tenant)
	return id
}

// get is the source's verifier, discovered on first use. A failed
// discovery is retried after discoveryRetry.
func (s *source) get(v *Verifier) (*oidc.IDTokenVerifier, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verifier != nil {
		return s.verifier, nil
	}
	if s.failure != nil && v.now().Sub(s.failedAt) < discoveryRetry {
		return nil, s.failure
	}
	// Not the request's context: one caller going away must not fail the
	// discovery the next one waits for, and the keys outlive the request.
	ctx, cancel := context.WithTimeout(oidc.ClientContext(context.Background(), v.client), discoveryTimeout)
	defer cancel()
	base := s.url
	if s.discovery != "" {
		// The document is not at the issuer's own URL (Entra v1.0): accept
		// it only for this exact issuer value.
		base, ctx = s.discovery, oidc.InsecureIssuerURLContext(ctx, s.url)
	}
	provider, err := oidc.NewProvider(ctx, base)
	if err != nil {
		s.failedAt, s.failure = v.now(), fmt.Errorf("discover issuer %s: %w", s.url, err)
		return nil, s.failure
	}
	s.failure = nil
	s.verifier = provider.Verifier(&oidc.Config{
		SkipClientIDCheck:    true, // several audiences: checked in Verify
		SupportedSigningAlgs: signingAlgorithms,
		Now:                  func() time.Time { return v.now().Add(-clockLeeway) },
	})
	return s.verifier, nil
}

// rejected is an Unauthenticated fault with a message for the caller; the
// reason stays in the error text, for the server's log.
func rejected(message string, reason error) error {
	return fmt.Errorf("%w: %w", fault.New(fault.Unauthenticated, message), reason)
}

// unverifiedIssuer reads the "iss" claim of a JWT without verifying it:
// only to choose the issuer whose keys then verify the token.
func unverifiedIssuer(raw string) (string, error) {
	if len(raw) > maxTokenBytes {
		return "", errors.New("token too long")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", errors.New("not three dot-separated parts")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("payload: %w", err)
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("payload: %w", err)
	}
	if claims.Issuer == "" {
		return "", errors.New("no iss claim")
	}
	return claims.Issuer, nil
}

// claimString is a string claim; "" for anything else.
func claimString(v any) string {
	s, _ := v.(string)
	return s
}

// claimStrings is a claim that is a string or a list of strings; other
// values and other elements are dropped.
func claimStrings(v any) []string {
	switch c := v.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []string{c}
	case []any:
		out := make([]string, 0, len(c))
		for _, e := range c {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
