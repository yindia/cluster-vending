package catalog_test

import (
	"fmt"
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
- {minor: "1.37", default: true, endOfSupport: "2028-01-31", channel: alpha}
components: {karpenter: "1.15.1"}
channels:
- {name: alpha, promotion: Pinned}
`
	const three = `
kubernetes:
- {minor: "1.37", default: true, endOfSupport: "2028-01-31", channel: %s}
- {minor: "1.36", endOfSupport: "2027-09-30", channel: %s}
- {minor: "1.35", endOfSupport: "2027-05-31", channel: %s}
- {minor: "1.34", endOfSupport: "2027-01-31"}
components: {karpenter: "1.15.1"}
channels:
- {name: alpha, promotion: Pinned}
- {name: beta, promotion: Automatic}
- {name: stable, promotion: Automatic}
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
		{"a release soak", strings.Replace(valid, "promotion: Pinned}", "promotion: Pinned, releaseSoak: soon}", 1),
			[]string{`releaseSoak "soon"`, "alpha is Pinned: a releaseSoak is for an Automatic channel"}},
		{"a minor's own version", `
kubernetes:
- {minor: "1.37", default: true, endOfSupport: "2028-01-31", components: {karpenter: "1.14.2"}}
components: {karpenter: "1.15.1"}
`, nil},
		{"a minor's version of an unknown component, and an empty one", `
kubernetes:
- {minor: "1.37", default: true, endOfSupport: "2028-01-31", components: {nope: "1.0.0", karpenter: ""}}
components: {karpenter: "1.15.1"}
`, []string{"kubernetes 1.37: components: nope has no version under components", "kubernetes 1.37: components: karpenter has no version"}},
		{"a channel per minor, and a minor on none", fmt.Sprintf(three, "alpha", "beta", "stable"), nil},
		{"a channel on no minor", fmt.Sprintf(three, "alpha", "beta", `""`),
			[]string{"channel stable is on no minor"}},
		{"a channel on two minors", fmt.Sprintf(three, "alpha", "beta", "beta"),
			[]string{"channel beta is on 1.36 and 1.35"}},
		{"stable newer than beta", fmt.Sprintf(three, "alpha", "stable", "beta"),
			[]string{"stable (1.36) is newer than beta (1.35)"}},
		{"a channel the file does not have", strings.Replace(valid, "channel: alpha", "channel: beta", 1),
			[]string{"kubernetes 1.37: channel beta is not under channels", "channel alpha is on no minor"}},
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

// A minor's components override the file's version on that minor only.
func TestVersionOn(t *testing.T) {
	rel, err := catalog.ParseRelease([]byte(`
kubernetes:
- {minor: "1.37", default: true, endOfSupport: "2028-01-31"}
- {minor: "1.35", endOfSupport: "2027-05-31", components: {karpenter: "1.14.2"}}
components: {karpenter: "1.15.1", external-secrets: "2.15.0"}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ minor, name, want string }{
		{"1.37", "karpenter", "1.15.1"},
		{"1.35", "karpenter", "1.14.2"},
		{"1.35", "external-secrets", "2.15.0"},
	} {
		if got := rel.VersionOn(rel.Kubernetes[map[string]int{"1.37": 0, "1.35": 1}[tt.minor]], tt.name); got != tt.want {
			t.Errorf("%s on %s = %s, want %s", tt.name, tt.minor, got, tt.want)
		}
	}
}

// Every shipped minor pins the AL2023 AMI release its Karpenter node
// classes use; a malformed pin is refused.
func TestShippedReleasePinsTheNodeAMI(t *testing.T) {
	rel, err := catalog.ShippedRelease()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range rel.Kubernetes {
		if k.NodeAMIPin == "" {
			t.Errorf("minor %s has no nodeAmiPin", k.Minor)
		}
	}
	const bad = `
kubernetes:
- {minor: "1.37", default: true, endOfSupport: "2028-01-31", channel: alpha, nodeAmiPin: latest}
components: {karpenter: "1.15.1"}
channels:
- {name: alpha, promotion: Pinned}
`
	if _, err := catalog.ParseRelease([]byte(bad)); err == nil || !strings.Contains(err.Error(), "nodeAmiPin") {
		t.Errorf("err = %v", err)
	}
}

// Karpenter installs first (sequence 0), cluster-autoscaler right after it
// (10): it waits for Karpenter only, never for a node group, and everything
// else (the default, 100) installs after both.
func TestKarpenterAndTheAutoscalerInstallFirst(t *testing.T) {
	defs, err := catalog.Definitions()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		got := int32(100)
		if d.Spec.Sequence != nil {
			got = *d.Spec.Sequence
		}
		want, ok := map[string]int32{"karpenter": 0, "cluster-autoscaler": 10}[d.Name]
		if !ok {
			want = 100
		}
		if got != want {
			t.Errorf("%s has sequence %d, want %d", d.Name, got, want)
		}
	}
}
