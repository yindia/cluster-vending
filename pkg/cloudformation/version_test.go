package cloudformation

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// goldenHashes maps each TemplateVersion to the SHA-256 of every embedded
// template of that version. A change of a template (its permissions, its
// trust, anything) changes its hash: bump TemplateVersion, regenerate
// (make cf-generate) and add the new version's hashes here, so that the
// account check can tell an operator whose stack is older to update it.
// Earlier versions stay as history.
var goldenHashes = map[int]map[string]string{
	1: {
		"reader":      "3f522670706901366c980d64f51c09fa62222644df8bf4967178d5fa954d5690",
		"updater":     "f2fb3ba901bd7b82d1e84308d53cf17a9b2f1ac3fbfd6ddfda9471a4ab8cdd33",
		"provisioner": "48463504a026badd79b792e8b1e8542c31ed46c251900e084031eb581f7294f2",
	},
	2: {
		"reader":      "a77bec1f0a520733ffc9ed46a4a100c37ee4ad0004c24358505505e251761c38",
		"updater":     "e81cfab5274d340cd76bc26a2b89bf3c9115e2b064a2e1e422c1d680d641cdba",
		"provisioner": "e2a408dad960d99aa60e783378e305cc9672c10c5dd3bd94e784090516c43f0c",
	},
}

// managementGoldenHashes is goldenHashes of the management account's
// templates, by ManagementTemplateVersion.
var managementGoldenHashes = map[int]map[string]string{
	1: {
		"management": "8c2d5659467d4256d2a30300006cdff373576d35b18dfd222a0a5521a305300a",
		"server":     "5f0e74cc9d129567b00ae5d03540f3c7a250cb0213ee0e8e38754076895b2d20",
	},
}

func hashOf(t *testing.T, role string) string {
	t.Helper()
	body, ok := anyTemplate(role)
	if !ok {
		t.Fatalf("no template of %s", role)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func TestManagementTemplatesChangeOnlyWithAVersionBump(t *testing.T) {
	golden, ok := managementGoldenHashes[ManagementTemplateVersion]
	if !ok {
		t.Fatalf("ManagementTemplateVersion %d has no entry in managementGoldenHashes", ManagementTemplateVersion)
	}
	for _, role := range managementRoles {
		if got := hashOf(t, role); got != golden[role] {
			t.Errorf("the %s template changed (sha256 %s, version %d expects %q): bump ManagementTemplateVersion in version.go, "+
				"run make cf-generate, and record the new hashes", role, got, ManagementTemplateVersion, golden[role])
		}
	}
}

func TestTemplatesChangeOnlyWithAVersionBump(t *testing.T) {
	golden, ok := goldenHashes[TemplateVersion]
	if !ok {
		t.Fatalf("TemplateVersion %d has no entry in goldenHashes: add the hashes of the templates of this version", TemplateVersion)
	}
	for _, role := range roles {
		if got := hashOf(t, role); got != golden[role] {
			t.Errorf("the %s template changed (sha256 %s, version %d expects %q): bump TemplateVersion in version.go, "+
				"run make cf-generate, and record the new hashes", role, got, TemplateVersion, golden[role])
		}
	}
}

func TestTemplatesCarryTheirVersion(t *testing.T) {
	for _, role := range slices.Concat(roles, managementRoles) {
		version := TemplateVersion
		if slices.Contains(managementRoles, role) {
			version = ManagementTemplateVersion
		}
		want := "Key: " + TagTemplateVersion + "\n          Value: '" + strconv.Itoa(version) + "'"
		body, _ := anyTemplate(role)
		if !strings.Contains(string(body), want) {
			t.Errorf("the %s template has no role tag %s", role, want)
		}
		if !strings.Contains(string(body), "TemplateVersion:") {
			t.Errorf("the %s template has no TemplateVersion output", role)
		}
	}
}

func TestVersionOfTags(t *testing.T) {
	tests := []struct {
		name string
		tags map[string]string
		want int
	}{
		{"current", map[string]string{TagTemplateVersion: strconv.Itoa(TemplateVersion)}, TemplateVersion},
		{"missing tag: before versions", map[string]string{"managed-by": Platform}, 0},
		{"no tags", nil, 0},
		{"not a number", map[string]string{TagTemplateVersion: "abc"}, 0},
		{"negative", map[string]string{TagTemplateVersion: "-3"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VersionOfTags(tt.tags); got != tt.want {
				t.Errorf("VersionOfTags = %d, want %d", got, tt.want)
			}
		})
	}
}
