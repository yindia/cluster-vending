// Package auth is who may call the API: the configuration of the OIDC
// issuers the server accepts tokens from (config.go), the verification of
// a bearer token (verify.go), the caller it stands for and what that
// caller may do (identity.go), and what each RPC needs (access.go). It
// knows no RPC library; pkg/server/authn applies it to requests.
package auth

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Mode is whether callers must authenticate.
type Mode string

// Modes.
const (
	// ModeNone accepts every request without looking at a token: the API
	// trusts the tenant a request names. For local testing only.
	ModeNone Mode = "none"
	// ModeOptional verifies a token when one is sent (a bad one is
	// refused) and lets requests without one through as ModeNone does: the
	// step between none and required.
	ModeOptional Mode = "optional"
	// ModeRequired refuses every request without an accepted token.
	ModeRequired Mode = "required"
)

// Role is what a caller may do.
type Role string

// Roles.
const (
	// RolePlatformAdmin may call everything, for every tenant.
	RolePlatformAdmin Role = "PlatformAdmin"
	// RoleTenantAdmin may call every tenant RPC of its tenants.
	RoleTenantAdmin Role = "TenantAdmin"
	// RoleTenantViewer may call the RPCs without side effects of its
	// tenants.
	RoleTenantViewer Role = "TenantViewer"
)

// AllTenants in a grant's tenants means every tenant.
const AllTenants = "*"

// Claims used when an issuer names none.
const (
	defaultUsernameClaim = "sub"
	defaultRolesClaim    = "groups"
	entraUsernameClaim   = "oid"
	entraRolesClaim      = "roles"
)

var (
	issuerNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tenantPattern     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,54}[a-z0-9])?$`)
	guidPattern       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Config is the "auth" section of the server's settings file. It holds no
// secret: verifying a token needs only the issuer's public keys.
type Config struct {
	// Mode defaults to none.
	Mode Mode `yaml:"mode"`
	// Issuers are the OIDC issuers whose tokens are accepted.
	Issuers []Issuer `yaml:"issuers"`
	// Rules say what the callers of an issuer may do. Every rule that
	// matches a caller adds its grant: there is no first match.
	Rules []Rule `yaml:"rules"`
}

// Issuer is one OIDC issuer.
type Issuer struct {
	// Name is how rules and logs refer to the issuer.
	Name string `yaml:"name"`
	// Issuer is its URL: the "iss" claim of its tokens, and where its
	// discovery document is (<issuer>/.well-known/openid-configuration).
	// Unset with entra: https://login.microsoftonline.com/<tenantId>/v2.0.
	Issuer string `yaml:"issuer"`
	// Audiences are the "aud" values accepted: a token must carry one.
	Audiences []string `yaml:"audiences"`
	// Entra marks a Microsoft Entra ID tenant (see Entra).
	Entra *Entra `yaml:"entra"`
	// UsernameClaim is the claim shown as the caller's name. Default "sub"
	// ("oid" with entra).
	UsernameClaim string `yaml:"usernameClaim"`
	// TenantClaim is a claim whose value is the caller's platform tenant:
	// a tenant rule without tenants grants its role for that tenant.
	TenantClaim string `yaml:"tenantClaim"`
	// RolesClaim is the claim rules match their role against: a string or
	// a list of strings. Default "groups" ("roles", the app roles, with
	// entra).
	RolesClaim string `yaml:"rolesClaim"`

	// Alternates are further "iss" values accepted for this issuer, each
	// with where its discovery document is. Set by presets (entra
	// acceptV1), not by the file.
	Alternates []Alternate `yaml:"-"`
}

// Entra is the Microsoft Entra ID preset of an issuer.
type Entra struct {
	// TenantID is the directory (tenant) ID, a GUID.
	TenantID string `yaml:"tenantId"`
	// AcceptV1 also accepts the tenant's v1.0 access tokens, whose issuer
	// is https://sts.windows.net/<tenantId>/ and whose audience may be
	// the API's Application ID URI: what Entra issues while the API's app
	// registration has accessTokenAcceptedVersion null or 1.
	AcceptV1 bool `yaml:"acceptV1"`
}

// Alternate is another issuer value of an Issuer.
type Alternate struct {
	// Issuer is the "iss" claim.
	Issuer string
	// DiscoveryURL is the base of its discovery document, which names
	// Issuer as its issuer.
	DiscoveryURL string
}

// Rule grants a role to the callers it matches.
type Rule struct {
	Match Match `yaml:"match"`
	Grant Grant `yaml:"grant"`
}

// Match selects callers.
type Match struct {
	// Issuer is an Issuer's name.
	Issuer string `yaml:"issuer"`
	// Role is a value of the issuer's roles claim. Unset: every caller of
	// the issuer.
	Role string `yaml:"role"`
}

// Grant is a role, for tenants unless it is PlatformAdmin.
type Grant struct {
	Role Role `yaml:"role"`
	// Tenants the role is for; AllTenants ("*") alone for every tenant.
	// Unset for a tenant role: the tenant in the issuer's tenantClaim.
	Tenants []string `yaml:"tenants"`
}

// Resolved returns the configuration with its defaults and presets
// applied, or every problem it has.
func (c Config) Resolved() (Config, error) {
	out := Config{Mode: c.Mode, Rules: slices.Clone(c.Rules)}
	if out.Mode == "" {
		out.Mode = ModeNone
	}
	var errs []error
	if !slices.Contains([]Mode{ModeNone, ModeOptional, ModeRequired}, out.Mode) {
		errs = append(errs, fmt.Errorf("auth.mode must be none, optional or required, got %q", out.Mode))
	}
	if out.Mode != ModeNone && len(c.Issuers) == 0 {
		errs = append(errs, fmt.Errorf("auth.mode %s needs at least one issuer in auth.issuers", out.Mode))
	}
	names := map[string]Issuer{}
	for i, issuer := range c.Issuers {
		resolved, problems := issuer.resolved()
		for _, p := range problems {
			errs = append(errs, fmt.Errorf("auth.issuers[%d]: %w", i, p))
		}
		if _, dup := names[resolved.Name]; dup && resolved.Name != "" {
			errs = append(errs, fmt.Errorf("auth.issuers[%d]: name %q is used twice", i, resolved.Name))
		}
		names[resolved.Name] = resolved
		out.Issuers = append(out.Issuers, resolved)
	}
	for i, rule := range out.Rules {
		for _, p := range rule.problems(names) {
			errs = append(errs, fmt.Errorf("auth.rules[%d]: %w", i, p))
		}
	}
	errs = append(errs, duplicateIssuerURLs(out.Issuers)...)
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return out, nil
}

// duplicateIssuerURLs reports "iss" values two issuers would both accept:
// a token names its issuer by that value alone.
func duplicateIssuerURLs(issuers []Issuer) []error {
	seen := map[string]string{}
	var errs []error
	for _, issuer := range issuers {
		for _, u := range issuer.urls() {
			if other, dup := seen[u]; dup {
				errs = append(errs, fmt.Errorf("auth.issuers: %s and %s both accept issuer %s", other, issuer.Name, u))
				continue
			}
			seen[u] = issuer.Name
		}
	}
	return errs
}

// urls are the "iss" values the issuer accepts.
func (i Issuer) urls() []string {
	out := make([]string, 0, 1+len(i.Alternates))
	out = append(out, i.Issuer)
	for _, a := range i.Alternates {
		out = append(out, a.Issuer)
	}
	return out
}

// resolved applies the issuer's defaults and its entra preset.
func (i Issuer) resolved() (Issuer, []error) {
	var errs []error
	out := i
	out.Audiences = slices.Clone(i.Audiences)
	if !issuerNamePattern.MatchString(i.Name) {
		errs = append(errs, fmt.Errorf("name %q must be lower-case letters, digits and dashes (at most 63)", i.Name))
	}
	usernameClaim, rolesClaim := defaultUsernameClaim, defaultRolesClaim
	if i.Entra != nil {
		usernameClaim, rolesClaim = entraUsernameClaim, entraRolesClaim
		errs = append(errs, out.applyEntra()...)
	}
	if out.UsernameClaim == "" {
		out.UsernameClaim = usernameClaim
	}
	if out.RolesClaim == "" {
		out.RolesClaim = rolesClaim
	}
	if err := checkIssuerURL(out.Issuer); err != nil {
		errs = append(errs, err)
	}
	if len(out.Audiences) == 0 {
		errs = append(errs, errors.New("audiences needs at least one value: the \"aud\" of the tokens issued for this API"))
	}
	if slices.Contains(out.Audiences, "") {
		errs = append(errs, errors.New("audiences has an empty value"))
	}
	return out, errs
}

// applyEntra sets what the entra preset decides: the v2.0 issuer and, with
// acceptV1, the v1.0 one.
func (i *Issuer) applyEntra() []error {
	tenant := strings.ToLower(i.Entra.TenantID)
	if !guidPattern.MatchString(tenant) {
		return []error{fmt.Errorf("entra.tenantId %q must be the directory (tenant) ID, a GUID", i.Entra.TenantID)}
	}
	v2 := EntraIssuer(tenant)
	if i.Issuer != "" && i.Issuer != v2 {
		return []error{fmt.Errorf("issuer %q is not the tenant's: with entra it is %s (leave it unset)", i.Issuer, v2)}
	}
	i.Issuer = v2
	if i.Entra.AcceptV1 {
		i.Alternates = []Alternate{{Issuer: EntraV1Issuer(tenant), DiscoveryURL: entraAuthority + tenant}}
	}
	return nil
}

// entraAuthority is where Entra tenants live.
const entraAuthority = "https://login.microsoftonline.com/"

// EntraIssuer is the issuer of a tenant's v2.0 tokens, and the base of its
// v2.0 discovery document.
func EntraIssuer(tenantID string) string { return entraAuthority + tenantID + "/v2.0" }

// EntraV1Issuer is the issuer of a tenant's v1.0 tokens.
func EntraV1Issuer(tenantID string) string { return "https://sts.windows.net/" + tenantID + "/" }

// checkIssuerURL accepts an https URL, or http on a loopback host (a
// local identity provider in development).
func checkIssuerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("issuer %q must be a URL without query or fragment, e.g. https://login.example.com/realms/platform", raw)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("issuer %q must use https (http is accepted on localhost only)", raw)
}

// isLoopback reports whether host names this machine.
func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// problems are what is wrong with the rule, given the issuers by name.
func (r Rule) problems(issuers map[string]Issuer) []error {
	var errs []error
	issuer, known := issuers[r.Match.Issuer]
	if !known || r.Match.Issuer == "" {
		errs = append(errs, fmt.Errorf("match.issuer %q is not the name of an issuer", r.Match.Issuer))
	}
	switch r.Grant.Role {
	case RolePlatformAdmin:
		if len(r.Grant.Tenants) > 0 {
			errs = append(errs, errors.New("grant.tenants must be unset for PlatformAdmin: it is for every tenant"))
		}
	case RoleTenantAdmin, RoleTenantViewer:
		errs = append(errs, tenantProblems(r.Grant.Tenants, known && issuer.TenantClaim != "")...)
	default:
		errs = append(errs, fmt.Errorf("grant.role must be %s, %s or %s, got %q",
			RolePlatformAdmin, RoleTenantAdmin, RoleTenantViewer, r.Grant.Role))
	}
	return errs
}

// tenantProblems checks a tenant role's tenants; fromClaim: the issuer
// has a tenant claim, so the list may be empty.
func tenantProblems(tenants []string, fromClaim bool) []error {
	if len(tenants) == 0 {
		if fromClaim {
			return nil
		}
		return []error{errors.New("grant.tenants needs the tenants the role is for (\"*\" for all), or the issuer a tenantClaim")}
	}
	if slices.Contains(tenants, AllTenants) {
		if len(tenants) > 1 {
			return []error{errors.New("grant.tenants: \"*\" stands alone")}
		}
		return nil
	}
	var errs []error
	for _, t := range tenants {
		if !tenantPattern.MatchString(t) {
			errs = append(errs, fmt.Errorf("grant.tenants: %q is not a tenant ID", t))
		}
	}
	return errs
}
