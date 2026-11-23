package catalog_test

import (
	"strings"
	"testing"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/catalog"
)

// withDefs is goodSet with more platform definitions, each pinned on
// every minor of the release.
func withDefs(defs ...cloudv1alpha1.ComponentDefinition) catalog.Set {
	s := goodSet()
	s.Definitions = append(s.Definitions, defs...)
	for i := range s.Releases[0].Spec.Content.Kubernetes {
		k := &s.Releases[0].Spec.Content.Kubernetes[i]
		for _, d := range defs {
			k.Components = append(k.Components, cloudv1alpha1.ComponentPin{Name: d.Name, Version: "1.0.0"})
		}
	}
	return s
}

func requires(seq int32, names ...string) func(*cloudv1alpha1.ComponentDefinition) {
	return func(d *cloudv1alpha1.ComponentDefinition) { d.Spec.Requires, d.Spec.Sequence = names, int32p(seq) }
}

// --check lists the dependents a disable switches off with it (directly
// and through others), and nothing for a disable nothing depends on.
func TestSwitchedOff(t *testing.T) {
	tests := []struct {
		name    string
		set     func() catalog.Set
		disable []string
		want    []string
	}{
		{"nothing disabled", goodSet, nil, nil},
		{"a disable nothing depends on", func() catalog.Set { return withDefs(mkDef("policy", requires(200, "karpenter"))) },
			[]string{"policy"}, nil},
		{"a disable with a direct dependent", func() catalog.Set {
			return withDefs(mkDef("policy", requires(200, "karpenter")), mkDef("reports", requires(300, "policy")))
		}, []string{"policy"}, []string{"ComponentDefinition/policy is disabled: it switches off reports, which depend on it"}},
		{"a chain of dependents", func() catalog.Set {
			return withDefs(mkDef("policy", requires(200, "karpenter")), mkDef("reports", requires(300, "policy")), mkDef("ui", requires(400, "reports")))
		}, []string{"policy"}, []string{"ComponentDefinition/policy is disabled: it switches off reports, ui, which depend on it"}},
		{"a dependent disabled itself is still listed", func() catalog.Set {
			return withDefs(mkDef("policy", requires(200, "karpenter")), mkDef("reports", requires(300, "policy")))
		}, []string{"policy", "reports"}, []string{"ComponentDefinition/policy is disabled: it switches off reports, which depend on it"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.set()
			for i := range s.Definitions {
				for _, name := range tt.disable {
					if s.Definitions[i].Name == name {
						s.Definitions[i].Spec.Disabled = true
					}
				}
			}
			got := catalog.SwitchedOff(s)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("notes = %q, want %q", got, tt.want)
			}
			if problems := catalog.Check(s, checkOptions); len(problems) > 0 {
				t.Errorf("a disable is no problem: %v", problemStrings(problems))
			}
		})
	}
}

// RunCheck prints the notes with a valid set, which still passes.
func TestRunCheckListsWhatADisableSwitchesOff(t *testing.T) {
	s := withDefs(mkDef("policy", func(d *cloudv1alpha1.ComponentDefinition) {
		requires(200, "karpenter")(d)
		d.Spec.Disabled = true
	}), mkDef("reports", requires(300, "policy")))
	var in, out strings.Builder
	if err := s.WriteStream(&in); err != nil {
		t.Fatal(err)
	}
	if err := catalog.RunCheck(strings.NewReader(in.String()), &out, checkOptions); err != nil {
		t.Fatalf("RunCheck: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "note: ComponentDefinition/policy is disabled: it switches off reports") ||
		!strings.Contains(out.String(), "catalog ok") {
		t.Errorf("output:\n%s", out.String())
	}
}
