package controller

import (
	"strings"
	"testing"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/release"
)

// A minor's own chart version reaches the Release on that minor only, and
// the release check still holds a version group together per minor.
func TestMinorVersionsReachTheRelease(t *testing.T) {
	defs, err := catalog.Definitions()
	if err != nil {
		t.Fatal(err)
	}
	items := make([]cloudv1alpha1.ComponentDefinition, 0, len(defs))
	for _, d := range defs {
		items = append(items, *d)
	}
	entries := CatalogEntries(items)
	parse := func(override string) cloudv1alpha1.ReleaseContent {
		t.Helper()
		rel, err := catalog.ParseRelease([]byte(`
kubernetes:
- {minor: "1.37", default: true, endOfSupport: "2028-01-31"}
- {minor: "1.36", endOfSupport: "2027-09-30", components: {` + override + `}}
components: {karpenter: "1.15.1", istio-base: "1.32.0", istiod: "1.32.0", istio-cni: "1.32.0", ztunnel: "1.32.0", istio-ingress-gateway: "1.32.0"}
`))
		if err != nil {
			t.Fatal(err)
		}
		return ContentOf(rel)
	}
	version := func(c cloudv1alpha1.ReleaseContent, minor, name string) string {
		for _, k := range c.Kubernetes {
			for _, p := range k.Components {
				if k.Minor == minor && p.Name == name {
					return p.Version
				}
			}
		}
		return ""
	}

	content := parse(`karpenter: "1.14.2"`)

	if got := version(content, "1.36", "karpenter"); got != "1.14.2" {
		t.Errorf("karpenter on 1.36 = %s, want 1.14.2", got)
	}
	if got := version(content, "1.37", "karpenter"); got != "1.15.1" {
		t.Errorf("karpenter on 1.37 = %s, want 1.15.1", got)
	}
	if p := release.CheckGroups(releasePins("r", content), entries); len(p) > 0 {
		t.Errorf("problems %v, want none", p)
	}

	split := parse(`istiod: "1.31.0"`)

	if p := strings.Join(release.CheckGroups(releasePins("r", split), entries), "; "); !strings.Contains(p, "1.36") || !strings.Contains(p, "istiod") {
		t.Errorf("problems %q, want the istio group split on 1.36", p)
	}
}
