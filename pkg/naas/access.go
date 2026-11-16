package naas

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/oidc"
)

const (
	WorkloadAdminRole  = "naas-workload-admin"
	WorkloadViewerRole = "naas-workload-viewer"
)

var ErrAccessOIDCNotConfigured = errors.New("SubTenant group access requires EKS OIDC with an issuer, client ID and groups claim")
var ErrLegacyAccessBindings = errors.New("legacy identityRef bindings require a trusted identity resolver; they are not OIDC group claims")

// GroupBinding is trusted template input, never a caller-selected role or subject.
type GroupBinding struct {
	ID, Group, Role string
}

// ResolvedAccess describes configuration only. An observed active, matching EKS
// association, delegation approval and current isolation qualification are still
// required before a controller may open access. This helper grants no access.
type ResolvedAccess struct {
	OIDCRevision string
	Bindings     []GroupBinding
}

// ValidateAccessGroups preserves case and exact claim values. Groups cannot
// occur twice, including across roles; sorting or prefixing never repairs input.
func ValidateAccessGroups(adminGroups, viewerGroups []string) error {
	seen := map[string]bool{}
	for _, groups := range [][]string{adminGroups, viewerGroups} {
		if len(groups) > 32 {
			return fmt.Errorf("each access role permits at most 32 groups")
		}
		for _, group := range groups {
			if !exactGroup(group) || utf8.RuneCountInString(group) > 256 {
				return fmt.Errorf("access groups must be exact nonreserved claim values of 1–256 characters without wildcards, edge whitespace or control characters")
			}
			if seen[group] {
				return fmt.Errorf("access groups must be unique within and across administrator/viewer roles")
			}
			seen[group] = true
		}
	}
	return nil
}

func exactGroup(group string) bool {
	return group != "" && utf8.ValidString(group) && group == strings.TrimSpace(group) &&
		!strings.HasPrefix(strings.ToLower(group), "system:") && !strings.ContainsAny(group, "*?") && !strings.ContainsFunc(group, unicode.IsControl)
}

// ResolveAccess translates raw token-claim values using exactly the same
// effective provider as the EKS adapter. Empty group intent needs no OIDC setup.
// Supplying an already-prefixed group is refused, never silently double-prefixed.
func ResolveAccess(access *cloud.SubTenantAccess, spec *cloudv1.ClusterSpec) (*ResolvedAccess, error) {
	if access == nil {
		return &ResolvedAccess{}, nil
	}
	if len(access.Bindings) != 0 {
		return nil, ErrLegacyAccessBindings
	}
	if err := ValidateAccessGroups(access.AdminGroups, access.ViewerGroups); err != nil {
		return nil, err
	}
	if len(access.AdminGroups)+len(access.ViewerGroups) == 0 {
		return &ResolvedAccess{}, nil
	}
	if spec == nil || spec.GetAws() == nil {
		return nil, ErrAccessOIDCNotConfigured
	}
	p := oidc.Resolve(spec.GetAccess().GetOidc())
	if p == nil || p.ClientID == "" || p.GroupsClaim == "" {
		return nil, ErrAccessOIDCNotConfigured
	}
	issuer, err := url.Parse(p.IssuerURL)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return nil, ErrAccessOIDCNotConfigured
	}
	if p.GroupsPrefix != "" && (!exactGroup(p.GroupsPrefix) || utf8.RuneCountInString(p.GroupsPrefix) > 255) {
		return nil, fmt.Errorf("effective OIDC groups prefix is unsafe")
	}
	// Bind to the complete effective authentication mapping, including required
	// claims. A changed issuer/audience/claim mapping requires new qualification.
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode effective OIDC settings: %w", err)
	}
	digest := sha256.Sum256(raw)
	resolved := &ResolvedAccess{OIDCRevision: "sha256:" + hex.EncodeToString(digest[:])}
	for _, role := range []struct {
		name   string
		groups []string
	}{{WorkloadAdminRole, access.AdminGroups}, {WorkloadViewerRole, access.ViewerGroups}} {
		for _, group := range role.groups {
			if p.GroupsPrefix != "" && strings.HasPrefix(group, p.GroupsPrefix) {
				return nil, fmt.Errorf("supply unprefixed OIDC group claims, not an already-prefixed subject")
			}
			effective := p.GroupsPrefix + group
			if !exactGroup(effective) {
				return nil, fmt.Errorf("effective OIDC group subject is unsafe")
			}
			id := sha256.Sum256([]byte(resolved.OIDCRevision + "\x00" + role.name + "\x00" + effective))
			resolved.Bindings = append(resolved.Bindings, GroupBinding{ID: hex.EncodeToString(id[:16]), Group: effective, Role: role.name})
		}
	}
	slices.SortFunc(resolved.Bindings, func(a, b GroupBinding) int { return strings.Compare(a.ID, b.ID) })
	return resolved, nil
}

// BindingData returns independent JSON-compatible GTR input. GTRs create one
// namespaced binding per stable ID in every namespace of this Capsule Tenant.
func (a *ResolvedAccess) BindingData() []any {
	data := make([]any, 0, len(a.Bindings))
	for _, binding := range a.Bindings {
		data = append(data, map[string]any{"id": binding.ID, "group": binding.Group, "role": binding.Role})
	}
	return data
}

// RenderTenantWithAccess adds validated group intent to the existing Tenant-only
// renderer. It never changes Capsule owners or opens either readiness label.
// Controllers must verify active OIDC and trusted qualification separately.
func RenderTenantWithAccess(sub *cloud.SubTenant, currentVersion, revision string, spec *cloudv1.ClusterSpec) (*unstructured.Unstructured, error) {
	access, err := ResolveAccess(sub.Spec.Access, spec)
	if err != nil {
		return nil, err
	}
	tenant, err := RenderTenant(sub, currentVersion, revision)
	if err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedSlice(tenant.Object, access.BindingData(), "spec", "data", "naas", "accessBindings"); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(tenant.Object, access.OIDCRevision, "spec", "data", "naas", "accessOIDCRevision"); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(tenant.Object["spec"])
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	annotations := tenant.GetAnnotations()
	annotations[SpecAnnotation] = hex.EncodeToString(digest[:])
	tenant.SetAnnotations(annotations)
	return tenant, nil
}
