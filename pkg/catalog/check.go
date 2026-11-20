package catalog

import (
	"slices"
	"strings"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/release"
)

// Options of Check.
type Options struct {
	// RenderVersion is the newest renderer version a release may name
	// (crossplaneaws.RenderVersion of the build that runs the check).
	RenderVersion int64
	// SystemGroup is nodeGroups.system of the config file; nil: the
	// sizing rule is not run.
	SystemGroup *config.SystemNodeGroup
	// PlatformPool is nodeGroups.platform of the config file; nil: the
	// platform pool's sizing rule is not run.
	PlatformPool *config.PlatformNodePool
}

// karpenterName is the one definition that must be first: every other
// platform component waits for the nodes it provides.
const karpenterName = "karpenter"

// Check makes the cross-object rules of the catalog (PROP-01 4.5) over a
// set, with no cluster call, and returns every problem in a stable order;
// none means the set is consistent. The rules are the ones the controller
// applies to the live objects, in pkg/release and pkg/component, and the
// sizing rule (sizing.go).
//
// Disabling a component switches its dependents off with it (component.Off):
// a disabled definition an enabled one requires is not an error.
func Check(set Set, opts Options) []Problem {
	var problems []Problem
	set = set.Sort()
	problems = append(problems, checkUnique(set)...)
	problems = append(problems, checkSchema(set)...)
	defs, defProblems := definitionsOf(set)
	problems = append(problems, defProblems...)
	problems = append(problems, checkDefinitions(defs)...)
	problems = append(problems, checkSequences(defs)...)
	problems = append(problems, checkReleases(set, defs, opts)...)
	problems = append(problems, checkChannels(set)...)
	if opts.SystemGroup != nil {
		sizing, _ := CheckSystemGroupSizing(defs, *opts.SystemGroup)
		for _, p := range sizing {
			problems = append(problems, Problem{Object: "set", Message: p})
		}
		bootstrap, _ := CheckBootstrapSizing(defs, *opts.SystemGroup)
		for _, p := range bootstrap {
			problems = append(problems, Problem{Object: "set", Message: p})
		}
	}
	if opts.PlatformPool != nil {
		platform, _ := CheckPlatformSizing(defs, *opts.PlatformPool)
		for _, p := range platform {
			problems = append(problems, Problem{Object: "set", Message: p})
		}
	}
	slices.SortStableFunc(problems, func(a, b Problem) int {
		if c := strings.Compare(a.Object, b.Object); c != 0 {
			return c
		}
		return strings.Compare(a.Message, b.Message)
	})
	return slices.Compact(problems)
}

// definitionsOf is the definitions as the component checks take them.
func definitionsOf(set Set) ([]component.Definition, []Problem) {
	var out []component.Definition
	var problems []Problem
	for i := range set.Definitions {
		d, err := component.DefinitionOf(&set.Definitions[i])
		if err != nil {
			problems = append(problems, Problem{Object: "ComponentDefinition/" + set.Definitions[i].Name, Message: err.Error()})
			continue
		}
		out = append(out, d)
	}
	return out, problems
}

// checkUnique: each object once, and the three channels, once each.
func checkUnique(set Set) []Problem {
	var problems []Problem
	seen := map[string]bool{}
	once := func(kind, name string) {
		key := kind + "/" + name
		if seen[key] {
			problems = append(problems, problemf(kind, name, "defined more than once"))
		}
		seen[key] = true
	}
	for _, d := range set.Definitions {
		once("ComponentDefinition", d.Name)
	}
	for _, r := range set.Releases {
		once("Release", r.Name)
	}
	known := []string{cloudv1alpha1.ChannelAlpha, cloudv1alpha1.ChannelBeta, cloudv1alpha1.ChannelStable}
	for _, c := range set.Channels {
		once("ReleaseChannel", c.Name)
		if !slices.Contains(known, c.Name) {
			problems = append(problems, problemf("ReleaseChannel", c.Name, "channels are alpha, beta and stable"))
		}
	}
	for _, name := range known {
		if !seen["ReleaseChannel/"+name] {
			problems = append(problems, problemf("ReleaseChannel", name, "the channel does not exist"))
		}
	}
	return problems
}

// checkSchema: every object carries this build's schema version.
func checkSchema(set Set) []Problem {
	var problems []Problem
	check := func(kind, name string, annotations map[string]string) {
		switch v, ok := annotations[SchemaAnnotation]; {
		case !ok:
			problems = append(problems, problemf(kind, name, "annotation %s is missing (the catalog base sets it; `controller --export` writes it)", SchemaAnnotation))
		case v != SchemaVersion:
			problems = append(problems, problemf(kind, name, "annotation %s is %q, this build reads %q", SchemaAnnotation, v, SchemaVersion))
		}
	}
	for _, d := range set.Definitions {
		check("ComponentDefinition", d.Name, d.Annotations)
	}
	for _, r := range set.Releases {
		check("Release", r.Name, r.Annotations)
	}
	for _, c := range set.Channels {
		check("ReleaseChannel", c.Name, c.Annotations)
	}
	return problems
}

// checkDefinitions: each definition's own checks (the Valid condition's),
// and the catalog's (requirements exist and do not cycle, bundles).
func checkDefinitions(defs []component.Definition) []Problem {
	var problems []Problem
	for _, d := range defs {
		for _, msg := range component.CheckDefinition(d) {
			problems = append(problems, problemf("ComponentDefinition", d.Name, "%s", msg))
		}
	}
	for _, msg := range component.CheckCatalog(defs) {
		problems = append(problems, Problem{Object: "set", Message: msg})
	}
	return problems
}

// checkSequences: sequence 0 is karpenter's alone, karpenter is on, and a
// platform component's sequence is not below that of what it requires.
func checkSequences(defs []component.Definition) []Problem {
	var problems []Problem
	bySeq := map[string]component.Definition{}
	for _, d := range defs {
		bySeq[d.Name] = d
	}
	for _, d := range defs {
		switch {
		case d.Name == karpenterName && d.Sequence != 0:
			problems = append(problems, problemf("ComponentDefinition", d.Name, "spec.sequence must be 0, not %d: every other platform component waits for its nodes", d.Sequence))
		case d.Name == karpenterName && d.Disabled:
			problems = append(problems, problemf("ComponentDefinition", d.Name, "karpenter cannot be disabled: every other platform component waits for its nodes"))
		case d.Name != karpenterName && d.Sequence == 0:
			problems = append(problems, problemf("ComponentDefinition", d.Name, "spec.sequence 0 is karpenter's: it installs before everything else"))
		}
		if !d.IsPlatform() {
			continue
		}
		for _, r := range d.Requires {
			if req, ok := bySeq[r]; ok && req.IsPlatform() && req.Sequence > d.Sequence {
				problems = append(problems, problemf("ComponentDefinition", d.Name, "spec.sequence %d is below that of %s (%d), which it requires: it must install after it", d.Sequence, r, req.Sequence))
			}
		}
	}
	return problems
}

// checkReleases: serials, content, bundles, version groups, and a pin for
// every component in use on every minor.
func checkReleases(set Set, defs []component.Definition, opts Options) []Problem {
	var problems []Problem
	entries := catalogEntries(set.Definitions)
	wanted := pinnedNames(set.Definitions, defs)
	serials := map[int64]string{}
	for _, r := range set.Releases {
		switch s := r.Spec.Serial; {
		case s == nil:
			problems = append(problems, problemf("Release", r.Name, "spec.serial is not set: a release needs one (`controller --export` numbers the ones that predate it)"))
		case *s < 1:
			problems = append(problems, problemf("Release", r.Name, "spec.serial %d is below 1", *s))
		default:
			if other, dup := serials[*s]; dup {
				problems = append(problems, problemf("Release", r.Name, "spec.serial %d is also the serial of Release/%s", *s, other))
			} else {
				serials[*s] = r.Name
			}
		}
		for _, msg := range ValidateRelease(r.Spec.Content, opts.RenderVersion) {
			problems = append(problems, problemf("Release", r.Name, "%s", msg))
		}
		pins := reconcile.ReleaseOf(r.Name, r.Spec.Content)
		for _, msg := range release.CheckBundles(pins, entries) {
			problems = append(problems, problemf("Release", r.Name, "%s", msg))
		}
		for _, msg := range release.CheckGroups(pins, entries) {
			problems = append(problems, problemf("Release", r.Name, "%s", msg))
		}
		for _, k := range r.Spec.Content.Kubernetes {
			pinned := map[string]bool{}
			for _, c := range k.Components {
				pinned[c.Name] = true
			}
			for _, name := range wanted {
				if !pinned[name] {
					problems = append(problems, problemf("Release", r.Name, "on %s the release does not pin %s, which a cluster on it installs", k.Minor, name))
				}
			}
		}
	}
	return problems
}

// pinnedNames are the names a release must pin on every minor: each
// definition in use (not off, component.Off) that installs a chart, and
// each companion chart under its own name. A bundle installs nothing; its
// members are the ones pinned.
func pinnedNames(raw []cloudv1alpha1.ComponentDefinition, defs []component.Definition) []string {
	off := component.Off(defs)
	var names []string
	for _, d := range raw {
		if _, isOff := off[d.Name]; isOff || d.Spec.Install == nil {
			continue
		}
		names = append(names, d.Name)
		if d.Spec.Install.Helm != nil {
			for _, c := range d.Spec.Install.Helm.Companions {
				names = append(names, c.Name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// catalogEntries are the definitions as the release checks take them.
func catalogEntries(defs []cloudv1alpha1.ComponentDefinition) []release.Definition {
	out := make([]release.Definition, 0, len(defs))
	for _, d := range defs {
		out = append(out, release.Definition{Name: d.Name, Requires: d.Spec.Requires,
			VersionGroup: d.Spec.VersionGroup, OneMinorAtATime: d.Spec.UpgradeOneMinorAtATime, Includes: d.Spec.Includes})
	}
	return out
}

// checkChannels: a channel's release is one of the set.
func checkChannels(set Set) []Problem {
	var problems []Problem
	names := map[string]bool{}
	for _, r := range set.Releases {
		names[r.Name] = true
	}
	for _, c := range set.Channels {
		if c.Spec.Release != "" && !names[c.Spec.Release] {
			problems = append(problems, problemf("ReleaseChannel", c.Name, "spec.release %s is not a Release of the set", c.Spec.Release))
		}
	}
	return problems
}
