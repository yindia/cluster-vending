package controller

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	shipped "github.com/yindia/cluster-vending/config/catalog"
	"github.com/yindia/cluster-vending/pkg/component"
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

// The cluster autoscaler's image is of the Kubernetes minor it is pinned
// under (v1.36.x under 1.36), and its definition leaves the tag to the pin.
func TestClusterAutoscalerImageFollowsTheMinor(t *testing.T) {
	for _, k := range DefaultRelease().Spec.Content.Kubernetes {
		found := false
		for _, c := range k.Components {
			if c.Name != "cluster-autoscaler" {
				continue
			}
			found = true
			values, err := component.PinValues(c.Values)
			if err != nil {
				t.Fatal(err)
			}
			image, _ := values["image"].(map[string]any)
			tag, _ := image["tag"].(string)
			if !strings.HasPrefix(tag, "v"+k.Minor+".") {
				t.Errorf("on %s the cluster autoscaler image is %q, want v%s.x", k.Minor, tag, k.Minor)
			}
		}
		if !found {
			t.Errorf("%s does not pin cluster-autoscaler", k.Minor)
		}
	}
	defs, _ := shippedCatalog(t)
	for _, d := range defs {
		if d.Name != "cluster-autoscaler" {
			continue
		}
		def, err := component.DefinitionOf(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, set := def.PlatformValues["image"]; set {
			t.Error("the definition sets image: the release pins the tag per Kubernetes minor")
		}
		pinned := component.WithPinValues(def, component.Values{"image": map[string]any{"tag": "v1.37.0"}})
		if touched := component.LockedPathsTouched(component.Values{"image": map[string]any{"tag": "x"}}, pinned.PlatformValues); len(touched) != 1 {
			t.Errorf("the pinned tag is not locked: %v", touched)
		}
	}
}

// A pin whose values are not an object, or name an unknown placeholder,
// makes the release invalid.
func TestValidateReleaseChecksPinValues(t *testing.T) {
	for _, tc := range []struct{ values, want string }{
		{`{"image":{"tag":"v1.37.0"}}`, ""},
		{`{"awsRegion":"${region}"}`, ""},
		{`["x"]`, "the values of cluster-autoscaler"},
		{`{"a":"${nope}"}`, "${nope}"},
	} {
		content := DefaultRelease().Spec.Content
		content.Kubernetes[0].Components = []cloudv1alpha1.ComponentPin{{Name: "cluster-autoscaler", Version: "9.60.0",
			Values: &runtime.RawExtension{Raw: []byte(tc.values)}}}
		got := strings.Join(pinValueProblems(content), "; ")
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q, want %q", tc.values, got, tc.want)
		}
	}
}
