package catalog_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/catalog"
)

// release.yaml is valid, and gives a version to exactly the components the
// catalog installs with a chart.
func TestShippedReleaseCoversTheCatalog(t *testing.T) {
	rel, err := catalog.ShippedRelease()
	if err != nil {
		t.Fatal(err)
	}
	defs, err := catalog.Definitions()
	if err != nil {
		t.Fatal(err)
	}
	var charts []string
	for _, d := range defs {
		if d.Spec.Install != nil {
			charts = append(charts, d.Name)
		}
	}

	if got := rel.ComponentNames(); !slices.Equal(got, charts) {
		t.Errorf("release.yaml has versions for %v, the catalog installs %v", got, charts)
	}
	if len(rel.Kubernetes) != 3 || len(rel.Channels) != 3 {
		t.Errorf("%d minors and %d channels, want 3 and 3", len(rel.Kubernetes), len(rel.Channels))
	}
}

func TestParseReleaseReportsEveryProblem(t *testing.T) {
	const valid = `
kubernetes:
- {minor: "1.37", default: true, endOfSupport: "2028-01-31"}
components: {karpenter: "1.15.1"}
channels:
- {name: alpha, promotion: Pinned}
`
	tests := []struct {
		name string
		file string
		want []string
	}{
		{"valid", valid, nil},
		{"an unknown key", valid + "extra: true\n", []string{"extra"}},
		{"no minors, no components", "channels: []\n", []string{"at least one minor", "components: at least one"}},
		{"two defaults and a bad day", `
kubernetes:
- {minor: "1.37", default: true, endOfSupport: "soon"}
- {minor: "1.36", default: true, endOfSupport: "2027-09-30", upgradeTo: "1.40"}
components: {karpenter: ""}
`, []string{"exactly one minor is the default", `endOfSupport "soon"`, "upgradeTo 1.40 is not on offer", "karpenter has no version"}},
		{"values for a component without a version", `
kubernetes:
- {minor: "1.37", default: true, endOfSupport: "2028-01-31", values: {nope: {a: 1}}}
components: {karpenter: "1.15.1"}
`, []string{"values for nope"}},
		{"a bad channel", valid + "- {name: gamma, promotion: Sometimes}\n", []string{`"gamma"`, `"Sometimes"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := catalog.ParseRelease([]byte(tt.file))

			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("ParseRelease: %v", err)
				}
				return
			}
			for _, want := range tt.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error %v does not name %s", err, want)
				}
			}
		})
	}
}
