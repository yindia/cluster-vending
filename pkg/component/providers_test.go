package component

import (
	"strings"
	"testing"
)

func TestCheckDefinitionNeedsAProvider(t *testing.T) {
	tests := []struct {
		name, provider, want string
	}{
		{"none", "", "provider: name the cloud"},
		{"unknown", "oracle", `provider: "oracle" is not aws, gcp or azure`},
		{"aws", "aws", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(CheckDefinition(Definition{Name: "x", Provider: tt.provider}), "; ")

			if tt.want == "" && got != "" || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("problems %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCovers(t *testing.T) {
	d := Definition{Provider: "aws"}
	if !d.Covers("aws") || d.Covers("gcp") || d.Covers("") {
		t.Fatalf("Covers wrong for %q", d.Provider)
	}
}

func TestCheckCatalogProviders(t *testing.T) {
	tests := []struct {
		name string
		defs []Definition
		want string
	}{
		{"a bundle and its member", []Definition{
			{Name: "mesh", Management: Platform, Includes: []string{"mesh-base"}, Provider: "aws"},
			{Name: "mesh-base", Management: Platform, Internal: true, HasInstall: true, Provider: "gcp"},
		}, "mesh is for aws but its member mesh-base is for gcp"},
		{"a requirement for another cloud", []Definition{
			{Name: "policies", Requires: []string{"engine"}, HasInstall: true, Provider: "gcp"},
			{Name: "engine", HasInstall: true, Provider: "aws"},
		}, "policies is for gcp but requires engine, which is for aws"},
		{"two definitions for one cloud install one chart into one namespace", []Definition{
			{Name: "gpu-operator", HasInstall: true, Chart: "https://helm.ngc.nvidia.com/nvidia gpu-operator", Namespace: "gpu-operator", Provider: "aws"},
			{Name: "gpu-operator-copy", HasInstall: true, Chart: "https://helm.ngc.nvidia.com/nvidia gpu-operator", Namespace: "gpu-operator", Provider: "aws"},
		}, "gpu-operator and gpu-operator-copy are both for aws and install https://helm.ngc.nvidia.com/nvidia gpu-operator into namespace gpu-operator"},
		{"variants for different clouds", []Definition{
			{Name: "gpu-operator", HasInstall: true, Chart: "r c", Namespace: "n", Provider: "aws"},
			{Name: "gpu-operator-gcp", HasInstall: true, Chart: "r c", Namespace: "n", Provider: "gcp"},
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(CheckCatalog(tt.defs), "; ")

			if tt.want == "" && got != "" || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("problems %q, want %q", got, tt.want)
			}
		})
	}
}

func TestForProvider(t *testing.T) {
	entries := []PlatformEntry{
		{Definition: Definition{Name: "karpenter", Provider: "aws"}},
		{Definition: Definition{Name: "kyverno-gcp", Provider: "gcp"}},
		{Definition: Definition{Name: "lbc", Provider: "aws"}},
	}

	kept, keptElsewhere := ForProvider(entries, "gcp", map[string]bool{"lbc": true})

	var names []string
	for _, e := range kept {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "kyverno-gcp,lbc" || strings.Join(keptElsewhere, ",") != "lbc" {
		t.Fatalf("kept %v, kept although not for gcp %v", names, keptElsewhere)
	}
	if len(entries) != 3 {
		t.Error("the input was changed")
	}
}
