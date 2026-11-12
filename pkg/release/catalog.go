package release

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Definition is what the release checks need of a catalog entry
// (ComponentDefinition).
type Definition struct {
	Name string
	// Requires are components installed, and upgraded, first.
	Requires []string
	// VersionGroup: every member a release pins is pinned at one version
	// per Kubernetes minor. Empty: none.
	VersionGroup string
	// OneMinorAtATime: an upgrade may move one minor version only.
	OneMinorAtATime bool
	// Includes are the members of a bundle: one component made of several
	// definitions. A release offers a bundle on a Kubernetes minor by
	// pinning every member. Empty: not a bundle.
	Includes []string
}

// CheckBundles reports the bundles r offers in part: on a Kubernetes
// minor, some members pinned and others not. A bundle with no member
// pinned on a minor is not offered there, which is fine. The messages
// suit a Valid condition.
func CheckBundles(r Release, defs []Definition) []string {
	var problems []string
	for _, k := range r.Kubernetes {
		pinned := components(r, k.Minor)
		for _, d := range defs {
			var missing []string
			for _, m := range d.Includes {
				if _, ok := pinned[m]; !ok {
					missing = append(missing, m)
				}
			}
			if len(missing) == 0 || len(missing) == len(d.Includes) {
				continue
			}
			slices.Sort(missing)
			problems = append(problems, fmt.Sprintf("on %s, %s is pinned in part: %s missing; a release pins every member of a bundle or none",
				k.Minor, d.Name, strings.Join(missing, ", ")))
		}
	}
	slices.Sort(problems)
	return problems
}

// CheckGroups reports the pins of r that break a version group: members
// of one group pinned at different versions on the same Kubernetes minor.
// Pins of components the catalog does not have are not its concern. The
// messages suit a Valid condition.
func CheckGroups(r Release, defs []Definition) []string {
	group := map[string]string{}
	for _, d := range defs {
		if d.VersionGroup != "" {
			group[d.Name] = d.VersionGroup
		}
	}
	var problems []string
	for _, k := range r.Kubernetes {
		versions := map[string]map[string][]string{} // group -> version -> components
		for _, c := range k.Components {
			g, ok := group[c.Name]
			if !ok {
				continue
			}
			if versions[g] == nil {
				versions[g] = map[string][]string{}
			}
			versions[g][c.Version] = append(versions[g][c.Version], c.Name)
		}
		for _, g := range sortedKeys(versions) {
			if len(versions[g]) < 2 {
				continue
			}
			var parts []string
			for _, v := range sortedKeys(versions[g]) {
				names := versions[g][v]
				slices.Sort(names)
				parts = append(parts, fmt.Sprintf("%s at %s", strings.Join(names, ", "), v))
			}
			problems = append(problems, fmt.Sprintf("on %s, version group %s is pinned at more than one version: %s",
				k.Minor, g, strings.Join(parts, "; ")))
		}
	}
	return problems
}

// CheckStep reports the pins of next that skip a minor version of a
// component that upgrades one minor at a time, relative to current: the
// release a channel offers now, and next the one it would move to. A
// component is compared on each Kubernetes minor both releases have; a
// cluster moving to a new Kubernetes minor gets the pin of that minor, so
// it is compared with the highest version current pins for the
// component. A component new in next, or a version that is not
// <major>.<minor>[.<patch>] (a leading v allowed), is not checked; a
// change of major version is reported. Nil when there is nothing to
// refuse.
func CheckStep(current, next Release, defs []Definition) []string {
	stepped := map[string]bool{}
	for _, d := range defs {
		if d.OneMinorAtATime {
			stepped[d.Name] = true
		}
	}
	var problems []string
	for _, k := range next.Kubernetes {
		for _, c := range k.Components {
			if !stepped[c.Name] {
				continue
			}
			from, ok := currentVersion(current, k.Minor, c.Name)
			if !ok {
				continue
			}
			if p := stepProblem(c.Name, k.Minor, from, c.Version); p != "" {
				problems = append(problems, p)
			}
		}
	}
	return problems
}

// currentVersion is the version current pins for component on Kubernetes
// minor m, or else its highest pin on any minor.
func currentVersion(current Release, m, component string) (string, bool) {
	if versions := components(current, m); versions != nil {
		if v, ok := versions[component]; ok {
			return v, true
		}
	}
	best, found := "", false
	for _, k := range current.Kubernetes {
		for _, c := range k.Components {
			if c.Name != component {
				continue
			}
			if !found || compareVersion(c.Version, best) > 0 {
				best, found = c.Version, true
			}
		}
	}
	return best, found
}

// stepProblem says why moving component from one version to another skips
// a minor; "" when it does not (or the versions cannot be compared).
func stepProblem(component, kubernetes, from, to string) string {
	fromMajor, fromMinor, okFrom := majorMinor(from)
	toMajor, toMinor, okTo := majorMinor(to)
	switch {
	case !okFrom || !okTo:
		return ""
	case toMajor != fromMajor:
		return fmt.Sprintf("on %s, %s moves from %s to %s: a change of major version; it upgrades one minor at a time",
			kubernetes, component, from, to)
	case toMinor > fromMinor+1:
		return fmt.Sprintf("on %s, %s moves from %s to %s, skipping %d minor version(s); it upgrades one minor at a time",
			kubernetes, component, from, to, toMinor-fromMinor-1)
	}
	return ""
}

// UpgradeOrder is the order in which components are installed and
// upgraded: every component after those it requires, and otherwise by
// name. A requirement the catalog does not have, or a cycle, is an error.
func UpgradeOrder(defs []Definition) ([]string, error) {
	byName := make(map[string]Definition, len(defs))
	for _, d := range defs {
		if _, dup := byName[d.Name]; dup {
			return nil, fmt.Errorf("component %s is in the catalog twice", d.Name)
		}
		byName[d.Name] = d
	}
	pending := map[string]int{}
	dependents := map[string][]string{}
	for _, d := range defs {
		for _, r := range d.Requires {
			if _, ok := byName[r]; !ok {
				return nil, fmt.Errorf("component %s requires %s, which the catalog does not have", d.Name, r)
			}
			pending[d.Name]++
			dependents[r] = append(dependents[r], d.Name)
		}
	}
	var ready, out []string
	for name := range byName {
		if pending[name] == 0 {
			ready = append(ready, name)
		}
	}
	for len(ready) > 0 {
		slices.Sort(ready)
		next := ready[0]
		ready = ready[1:]
		out = append(out, next)
		for _, dep := range dependents[next] {
			if pending[dep]--; pending[dep] == 0 {
				ready = append(ready, dep)
			}
		}
	}
	if len(out) != len(byName) {
		var cycle []string
		for name, n := range pending {
			if n > 0 {
				cycle = append(cycle, name)
			}
		}
		slices.Sort(cycle)
		return nil, fmt.Errorf("components require each other in a cycle: %s", strings.Join(cycle, ", "))
	}
	return out, nil
}

// majorMinor reads "1.32", "1.32.0" or "v26.7.2".
func majorMinor(v string) (int, int, bool) {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	return major, minor, err1 == nil && err2 == nil
}

// compareVersion orders versions by major and minor, then as strings.
func compareVersion(a, b string) int {
	am, an, okA := majorMinor(a)
	bm, bn, okB := majorMinor(b)
	switch {
	case !okA || !okB:
		return strings.Compare(a, b)
	case am != bm:
		return am - bm
	case an != bn:
		return an - bn
	}
	return strings.Compare(a, b)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
