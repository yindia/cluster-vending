package controller

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	shipped "github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/release"
)

// shippedCatalog is the catalog this build ships, as the release checks
// take it.
func shippedCatalog(t *testing.T) ([]*cloudv1alpha1.ComponentDefinition, []release.Definition) {
	t.Helper()
	defs, err := shipped.Definitions()
	if err != nil {
		t.Fatal(err)
	}
	items := make([]cloudv1alpha1.ComponentDefinition, 0, len(defs))
	for _, d := range defs {
		items = append(items, *d)
	}
	return defs, CatalogEntries(items)
}

// The shipped release pins every installable shipped component on every
// minor, nothing the catalog does not have, and keeps the version groups.
func TestShippedReleasePinsTheShippedCatalog(t *testing.T) {
	defs, entries := shippedCatalog(t)
	var want []string
	for _, d := range defs {
		if d.Spec.Install != nil {
			want = append(want, d.Name)
			// A companion chart is pinned under its own name.
			for _, c := range d.Spec.Install.Helm.Companions {
				want = append(want, c.Name)
			}
		}
	}
	slices.Sort(want)
	rel := DefaultRelease()
	for _, k := range rel.Spec.Content.Kubernetes {
		var got []string
		for _, c := range k.Components {
			got = append(got, c.Name)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s pins %v, the catalog has %v", k.Minor, got, want)
		}
	}
	if problems := release.CheckGroups(releasePins(rel.Name, rel.Spec.Content), entries); len(problems) > 0 {
		t.Errorf("version groups: %v", problems)
	}
}

// A pin whose values are not an object, or name an unknown placeholder,
// makes the release invalid.
func TestValidateReleaseChecksPinValues(t *testing.T) {
	for _, tc := range []struct{ values, want string }{
		{`{"image":{"tag":"v1.37.0"}}`, ""},
		{`{"awsRegion":"${region}"}`, ""},
		{`["x"]`, "the values of karpenter"},
		{`{"a":"${nope}"}`, "${nope}"},
	} {
		content := DefaultRelease().Spec.Content
		content.Kubernetes[0].Components = []cloudv1alpha1.ComponentPin{{Name: "karpenter", Version: "1.15.1",
			Values: &runtime.RawExtension{Raw: []byte(tc.values)}}}
		got := strings.Join(pinValueProblems(content), "; ")
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q, want %q", tc.values, got, tc.want)
		}
	}
}
