package catalog_test

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// A small catalog that passes every rule: karpenter, the NodeProvisioner, and a chain of two
// platform components, one tenant component, one release on three minors
// pinning each of them, and the three channels.

func int32p(n int32) *int32 { return &n }
func int64p(n int64) *int64 { return &n }

func mkDef(name string, mutate func(*cloudv1alpha1.ComponentDefinition)) cloudv1alpha1.ComponentDefinition {
	d := cloudv1alpha1.ComponentDefinition{
		TypeMeta:   metav1.TypeMeta{APIVersion: cloudv1alpha1.GroupVersion.String(), Kind: "ComponentDefinition"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{catalog.SchemaAnnotation: catalog.SchemaVersion}},
		Spec: cloudv1alpha1.ComponentDefinitionSpec{Description: name, Provider: cloudv1alpha1.CloudProviderAWS,
			Management: cloudv1alpha1.ComponentManagementPlatform, Namespace: name, Sequence: int32p(100),
			Install: &cloudv1alpha1.ComponentInstall{Helm: &cloudv1alpha1.HelmInstall{
				HelmChartSource: cloudv1alpha1.HelmChartSource{Repository: "https://charts.example.com", Chart: name}}}},
	}
	if mutate != nil {
		mutate(&d)
	}
	return d
}

// withValues sets a definition's platform values from JSON.
func withValues(d *cloudv1alpha1.ComponentDefinition, raw string) {
	d.Spec.PlatformValues = &runtime.RawExtension{Raw: []byte(raw)}
}

func mkRelease(name string, serial int64, mutate func(*cloudv1alpha1.Release)) cloudv1alpha1.Release {
	eos := metav1.NewTime(time.Date(2027, 9, 30, 0, 0, 0, 0, time.UTC))
	pins := func() []cloudv1alpha1.ComponentPin {
		return []cloudv1alpha1.ComponentPin{{Name: "karpenter", Version: "1.0.0"}, {Name: "mesh", Version: "2.0.0"}, {Name: "mesh-cni", Version: "2.0.0"}, {Name: "tool", Version: "3.0.0"}}
	}
	r := cloudv1alpha1.Release{
		TypeMeta:   metav1.TypeMeta{APIVersion: cloudv1alpha1.GroupVersion.String(), Kind: "Release"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{catalog.SchemaAnnotation: catalog.SchemaVersion}},
		Spec: cloudv1alpha1.ReleaseSpec{Serial: int64p(serial), Content: cloudv1alpha1.ReleaseContent{RenderVersion: 1, Kubernetes: []cloudv1alpha1.KubernetesRelease{
			{Minor: "1.37", Channel: "alpha", Default: true, EndOfSupport: eos, Components: pins()},
			{Minor: "1.36", Channel: "beta", UpgradeTo: "1.37", EndOfSupport: eos, Components: pins()},
			{Minor: "1.35", Channel: "stable", UpgradeTo: "1.36", EndOfSupport: eos, Components: pins()},
		}}},
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func mkChannel(name string, mutate func(*cloudv1alpha1.ReleaseChannel)) cloudv1alpha1.ReleaseChannel {
	c := cloudv1alpha1.ReleaseChannel{
		TypeMeta:   metav1.TypeMeta{APIVersion: cloudv1alpha1.GroupVersion.String(), Kind: "ReleaseChannel"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{catalog.SchemaAnnotation: catalog.SchemaVersion}},
		Spec:       cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.PromotionAutomatic},
	}
	if name == "alpha" {
		c.Spec.Promotion, c.Spec.Release = cloudv1alpha1.PromotionPinned, "2026-11-a"
	}
	if mutate != nil {
		mutate(&c)
	}
	return c
}

func goodSet() catalog.Set {
	return catalog.Set{
		Definitions: []cloudv1alpha1.ComponentDefinition{
			mkDef("karpenter", func(d *cloudv1alpha1.ComponentDefinition) { d.Spec.Role = cloudv1alpha1.ComponentRoleNodeProvisioner }),
			mkDef("mesh", func(d *cloudv1alpha1.ComponentDefinition) {
				d.Spec.Requires = []string{"karpenter"}
				d.Spec.Install.Helm.Companions = []cloudv1alpha1.HelmCompanion{{Name: "mesh-cni",
					HelmChartSource: cloudv1alpha1.HelmChartSource{Repository: "https://charts.example.com", Chart: "mesh-cni"}}}
			}),
			mkDef("tool", func(d *cloudv1alpha1.ComponentDefinition) {
				d.Spec.Management = cloudv1alpha1.ComponentManagementTenant
			}),
		},
		Releases: []cloudv1alpha1.Release{mkRelease("2026-11-a", 1, nil)},
		Channels: []cloudv1alpha1.ReleaseChannel{mkChannel("alpha", nil), mkChannel("beta", nil), mkChannel("stable", nil)},
	}
}

var checkOptions = catalog.Options{RenderVersion: crossplaneaws.RenderVersion}

func TestAGoodSetPasses(t *testing.T) {
	if problems := catalog.Check(goodSet(), checkOptions); len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
}

// One row per rule: the mutation breaks exactly that rule, and the problem
// names the object and the rule.
func TestCheckRules(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*catalog.Set)
		want   []string // substrings of the problems, in any order; each must appear
	}{
		{"a channel is missing", func(s *catalog.Set) { s.Channels = s.Channels[:2] }, []string{"ReleaseChannel/stable: the channel does not exist"}},
		{"a channel twice", func(s *catalog.Set) { s.Channels = append(s.Channels, mkChannel("beta", nil)) }, []string{"ReleaseChannel/beta: defined more than once"}},
		{"an unknown channel", func(s *catalog.Set) { s.Channels = append(s.Channels, mkChannel("gamma", nil)) }, []string{"ReleaseChannel/gamma: channels are alpha, beta and stable"}},
		{"a channel names a release that is not in the set", func(s *catalog.Set) { s.Channels[0].Spec.Release = "2026-12-z" },
			[]string{"ReleaseChannel/alpha: spec.release 2026-12-z is not a Release of the set"}},
		{"a release twice", func(s *catalog.Set) { s.Releases = append(s.Releases, mkRelease("2026-11-a", 2, nil)) }, []string{"Release/2026-11-a: defined more than once"}},
		{"a requirement that does not exist", func(s *catalog.Set) { s.Definitions[1].Spec.Requires = []string{"ghost"} }, []string{"ghost"}},
		{"a requirement cycle", func(s *catalog.Set) {
			s.Definitions[1].Spec.Requires = []string{"tool2"}
			s.Definitions = append(s.Definitions, mkDef("tool2", func(d *cloudv1alpha1.ComponentDefinition) { d.Spec.Requires = []string{"mesh"} }))
			s.Releases[0] = mkRelease("2026-11-a", 1, func(r *cloudv1alpha1.Release) {
				for i := range r.Spec.Content.Kubernetes {
					r.Spec.Content.Kubernetes[i].Components = append(r.Spec.Content.Kubernetes[i].Components, cloudv1alpha1.ComponentPin{Name: "tool2", Version: "1"},
						cloudv1alpha1.ComponentPin{Name: "mesh-cni", Version: "2.0.0"})
				}
			})
		}, []string{"cycle"}},
		{"a requirement with a later sequence", func(s *catalog.Set) {
			s.Definitions[1].Spec.Sequence = int32p(50)
			s.Definitions = append(s.Definitions, mkDef("late", func(d *cloudv1alpha1.ComponentDefinition) { d.Spec.Sequence = int32p(200) }))
			s.Definitions[1].Spec.Requires = append(s.Definitions[1].Spec.Requires, "late")
			s.Releases[0] = mkRelease("2026-11-a", 1, func(r *cloudv1alpha1.Release) {
				for i := range r.Spec.Content.Kubernetes {
					r.Spec.Content.Kubernetes[i].Components = append(r.Spec.Content.Kubernetes[i].Components, cloudv1alpha1.ComponentPin{Name: "late", Version: "1"})
				}
			})
		}, []string{"ComponentDefinition/mesh: spec.sequence 50 is below that of late (200), which it requires"}},
		{"another definition with sequence 0", func(s *catalog.Set) { s.Definitions[1].Spec.Sequence = int32p(0) },
			[]string{"ComponentDefinition/mesh: spec.sequence 0 is the NodeProvisioner's"}},
		{"a karpenter stored before spec.role not at sequence 0", func(s *catalog.Set) { s.Definitions[0].Spec.Role = "" },
			[]string{"ComponentDefinition/karpenter: spec.sequence must be 0"}},
		{"two NodeProvisioners on one cloud", func(s *catalog.Set) { s.Definitions[1].Spec.Role = cloudv1alpha1.ComponentRoleNodeProvisioner },
			[]string{"karpenter and mesh both declare role NodeProvisioner on aws"}},
		{"a role on a Tenant component", func(s *catalog.Set) { s.Definitions[2].Spec.Role = cloudv1alpha1.ComponentRoleNamespaceTenancy },
			[]string{"tool: role NamespaceTenancy is for a Platform component only"}},
		{"karpenter on the platform pool", func(s *catalog.Set) { withValues(&s.Definitions[0], `{"nodeSelector":"${platform.nodeSelector}"}`) },
			[]string{"ComponentDefinition/karpenter: the NodeProvisioner must not run on the platform pool"}},
		{"another definition on the system group", func(s *catalog.Set) {
			withValues(&s.Definitions[1], `{"nodeSelector":"${system.nodeSelector}","tolerations":"${system.tolerations}"}`)
		}, []string{"ComponentDefinition/mesh: only the NodeProvisioner runs on the system group"}},
		{"a definition on both pools", func(s *catalog.Set) {
			withValues(&s.Definitions[1], `{"nodeSelector":"${system.nodeSelector}","tolerations":"${platform.tolerations}"}`)
		}, []string{"ComponentDefinition/mesh: mesh uses both ${system.*} and ${platform.*}"}},
		{"a system-group component requires one on the platform pool", func(s *catalog.Set) {
			withValues(&s.Definitions[1], `{"nodeSelector":"${system.nodeSelector}","tolerations":"${system.tolerations}"}`)
			s.Definitions = append(s.Definitions, mkDef("pooled", func(d *cloudv1alpha1.ComponentDefinition) {
				withValues(d, `{"nodeSelector":"${platform.nodeSelector}","tolerations":"${platform.tolerations}"}`)
			}))
			s.Definitions[1].Spec.Requires = append(s.Definitions[1].Spec.Requires, "pooled")
			s.Releases[0] = mkRelease("2026-11-a", 1, func(r *cloudv1alpha1.Release) {
				for i := range r.Spec.Content.Kubernetes {
					r.Spec.Content.Kubernetes[i].Components = append(r.Spec.Content.Kubernetes[i].Components, cloudv1alpha1.ComponentPin{Name: "pooled", Version: "1"})
				}
			})
		}, []string{"ComponentDefinition/mesh: runs on the system group (phase 1) but requires pooled, which runs on the platform pool (phase 2)"}},
		{"karpenter disabled", func(s *catalog.Set) { s.Definitions[0].Spec.Disabled = true },
			[]string{"ComponentDefinition/karpenter: the NodeProvisioner cannot be disabled"}},
		{"a release without a serial", func(s *catalog.Set) { s.Releases[0].Spec.Serial = nil }, []string{"Release/2026-11-a: spec.serial is not set"}},
		{"a serial of 0", func(s *catalog.Set) { s.Releases[0].Spec.Serial = int64p(0) }, []string{"Release/2026-11-a: spec.serial 0 is below 1"}},
		{"serials that tie", func(s *catalog.Set) { s.Releases = append(s.Releases, mkRelease("2026-11-b", 1, nil)) },
			[]string{"Release/2026-11-b: spec.serial 1 is also the serial of Release/2026-11-a"}},
		{"minors that are not consecutive", func(s *catalog.Set) { s.Releases[0].Spec.Content.Kubernetes[1].Minor = "1.30" }, []string{"Release/2026-11-a:", "not consecutive"}},
		{"an upgradeTo that is not on offer", func(s *catalog.Set) { s.Releases[0].Spec.Content.Kubernetes[2].UpgradeTo = "1.38" }, []string{"1.35 upgrades to 1.38"}},
		{"two minors on one channel", func(s *catalog.Set) { s.Releases[0].Spec.Content.Kubernetes[2].Channel = "beta" }, []string{"channel beta is on"}},
		{"a renderer newer than this build", func(s *catalog.Set) { s.Releases[0].Spec.Content.RenderVersion = 999 }, []string{"renderVersion 999 is newer"}},
		{"a pin missing on a minor", func(s *catalog.Set) {
			k := &s.Releases[0].Spec.Content.Kubernetes[1]
			k.Components = k.Components[:len(k.Components)-1]
		}, []string{"Release/2026-11-a: on 1.36 the release does not pin tool"}},
		{"a companion chart not pinned", func(s *catalog.Set) {
			k := &s.Releases[0].Spec.Content.Kubernetes[0]
			k.Components = k.Components[:2]
		}, []string{"on 1.37 the release does not pin mesh-cni", "on 1.37 the release does not pin tool"}},
		{"a disabled component needs no pin", func(s *catalog.Set) {
			s.Definitions[2].Spec.Disabled = true
			for i := range s.Releases[0].Spec.Content.Kubernetes {
				k := &s.Releases[0].Spec.Content.Kubernetes[i]
				k.Components = k.Components[:3]
			}
		}, nil},
		{"a version group split", func(s *catalog.Set) {
			s.Definitions[1].Spec.VersionGroup = "g"
			s.Definitions[2].Spec.VersionGroup = "g"
		}, []string{"version group g"}},
		{"a definition that fails its checks", func(s *catalog.Set) {
			s.Definitions[2].Spec.PlatformValues = &runtime.RawExtension{Raw: []byte(`{"a":"${no.such.placeholder}"}`)}
		}, []string{"ComponentDefinition/tool: platformValues"}},
		{"a definition without the schema annotation", func(s *catalog.Set) { delete(s.Definitions[2].Annotations, catalog.SchemaAnnotation) },
			[]string{"ComponentDefinition/tool: annotation " + catalog.SchemaAnnotation + " is missing"}},
		{"a newer schema", func(s *catalog.Set) { s.Channels[1].Annotations[catalog.SchemaAnnotation] = "2" },
			[]string{"ReleaseChannel/beta: annotation " + catalog.SchemaAnnotation + " is \"2\""}},
		// Decision 1: disabling a component switches its dependents off with
		// it; that is not an error.
		{"a disabled component with an enabled dependent is not an error", func(s *catalog.Set) {
			s.Definitions[1].Spec.Disabled = true
			for i := range s.Releases[0].Spec.Content.Kubernetes {
				k := &s.Releases[0].Spec.Content.Kubernetes[i]
				k.Components = []cloudv1alpha1.ComponentPin{{Name: "karpenter", Version: "1"}, {Name: "tool", Version: "1"}}
			}
		}, nil},
	}
	for _, tt := range tests {
		s := goodSet()
		tt.mutate(&s)
		got := catalog.Check(s, checkOptions)
		text := strings.Join(problemStrings(got), "\n")
		if len(tt.want) == 0 && len(got) > 0 {
			t.Errorf("%s: problems = %v, want none", tt.name, text)
		}
		for _, w := range tt.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: problems =\n%s\nwant one with %q", tt.name, text, w)
			}
		}
	}
}

func problemStrings(ps []catalog.Problem) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

// Every problem is listed, not the first.
func TestCheckListsEveryProblem(t *testing.T) {
	s := goodSet()
	s.Channels = s.Channels[:1]                             // beta, stable missing
	s.Definitions[0].Spec.Role = ""                         // karpenter as stored before spec.role,
	s.Definitions[0].Spec.Sequence = int32p(5)              // not at 0
	s.Releases[0].Spec.Serial = nil                         // serial
	s.Releases[0].Spec.Content.Kubernetes[1].Minor = "1.30" // minors
	got := catalog.Check(s, checkOptions)
	if len(got) < 5 {
		t.Fatalf("%d problems, want at least 5: %v", len(got), problemStrings(got))
	}
}

// The problems come in a stable order.
func TestCheckOrderIsStable(t *testing.T) {
	s := goodSet()
	s.Channels = s.Channels[:1]
	s.Releases[0].Spec.Serial = nil
	first := problemStrings(catalog.Check(s, checkOptions))
	for i := 0; i < 5; i++ {
		if again := problemStrings(catalog.Check(s, checkOptions)); strings.Join(again, "\n") != strings.Join(first, "\n") {
			t.Fatalf("order changed:\n%v\n%v", first, again)
		}
	}
}

func TestCheckSystemGroupSizingRule(t *testing.T) {
	s := goodSet()
	small := config.SystemNodeGroup{InstanceTypes: []string{"t3.small"}, MinSize: 1}
	opts := checkOptions
	opts.SystemGroup = &small
	// karpenter's requests are not set in the small catalog: the rule says so.
	s.Definitions[0].Spec.Sizing = karpenterSizing()
	text := strings.Join(problemStrings(catalog.Check(s, opts)), "\n")
	if !strings.Contains(text, "karpenter: platformValues has no") {
		t.Errorf("problems = %s, want the missing requests of karpenter", text)
	}
	// Without a config file the rule is not run.
	if problems := catalog.Check(s, checkOptions); len(problems) > 0 {
		t.Errorf("without nodeGroups: %v", problems)
	}
}

// Releases with the same content, created again, check the same: the check
// never reads a creation time.
func TestCheckIgnoresCreationTimes(t *testing.T) {
	s := goodSet()
	s.Releases[0].CreationTimestamp = metav1.NewTime(time.Now())
	if problems := catalog.Check(s, checkOptions); len(problems) > 0 {
		t.Fatal(problemStrings(problems))
	}
}

// `--check` prints every problem and fails; a good catalog prints ok.
func TestRunCheck(t *testing.T) {
	var good strings.Builder
	if err := goodSet().WriteStream(&good); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := catalog.RunCheck(strings.NewReader(good.String()), &out, checkOptions); err != nil || !strings.Contains(out.String(), "catalog ok: 3 definitions, 1 releases, 3 channels") {
		t.Fatalf("good catalog: %v, %q", err, out.String())
	}

	bad := goodSet()
	bad.Channels = bad.Channels[:1]
	bad.Releases[0].Spec.Serial = nil
	bad.Definitions[0].Spec.Role = "" // as stored before spec.role, not at 0
	bad.Definitions[0].Spec.Sequence = int32p(7)
	var stream strings.Builder
	if err := bad.WriteStream(&stream); err != nil {
		t.Fatal(err)
	}
	stream.WriteString("---\napiVersion: cloud.cluster-vending.io/v1alpha1\nkind: ReleaseChannel\nmetadata: {name: beta}\nspec: {bogus: 1}\n")
	out.Reset()
	err := catalog.RunCheck(strings.NewReader(stream.String()), &out, checkOptions)
	if err == nil || !strings.Contains(err.Error(), "problems") {
		t.Fatalf("err = %v, want a failure", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 4 {
		t.Fatalf("%d lines, want every problem:\n%s", len(lines), out.String())
	}
	for _, want := range []string{"ReleaseChannel/beta: cannot be read", "ReleaseChannel/stable: the channel does not exist",
		"Release/2026-11-a: spec.serial is not set", "ComponentDefinition/karpenter: spec.sequence must be 0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(err.Error(), "problems") || strings.Contains(err.Error(), "1 problem ") {
		t.Errorf("err = %v", err)
	}
}
