package component

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/yindia/cluster-vending/pkg/fault"
	"github.com/yindia/cluster-vending/pkg/release"
)

// Management says who controls a component.
type Management string

// Who controls a component.
const (
	// Platform: installed with every cluster by the cluster's reconcile,
	// with fixed values. No Component object exists for it.
	Platform Management = "Platform"
	// Tenant: a tenant enables and disables it on a cluster. The default:
	// an empty Management is Tenant.
	Tenant Management = "Tenant"
)

// IsPlatform reports whether the platform installs d with every cluster.
func (d Definition) IsPlatform() bool { return d.Management == Platform }

// IsBundle reports whether d is one component made of several definitions.
func (d Definition) IsBundle() bool { return len(d.Includes) > 0 }

// CheckTenantAccess refuses what a tenant may not enable, update or
// disable through the Component API, with a PermissionDenied fault naming
// the component: a Platform component (it is installed with the cluster),
// and an internal one (it is part of a bundle; catalog names the bundle
// when it holds one that includes it). Nil for a Tenant, non-internal
// definition.
func CheckTenantAccess(d Definition, catalog []Definition) error {
	if d.IsPlatform() {
		return fault.Newf(fault.PermissionDenied,
			"component %s is installed with the cluster by the platform; it cannot be enabled, changed or disabled", d.Name)
	}
	if !d.Internal {
		return nil
	}
	var bundles []string
	for _, b := range catalog {
		if slices.Contains(b.Includes, d.Name) {
			bundles = append(bundles, b.Name)
		}
	}
	if len(bundles) == 0 {
		return fault.Newf(fault.PermissionDenied, "component %s is internal to the platform; it cannot be enabled by itself", d.Name)
	}
	slices.Sort(bundles)
	return fault.Newf(fault.PermissionDenied, "component %s is part of %s; enable, change or disable %s instead",
		d.Name, strings.Join(bundles, ", "), strings.Join(bundles, ", "))
}

// CheckCatalog reports what is wrong between the definitions of a catalog,
// which no definition can be judged for alone, sorted; nil when the
// catalog is consistent:
//   - a Platform definition requires only Platform definitions;
//   - a bundle includes only definitions the catalog has that are internal and not
//     bundles, and a Platform bundle only Platform members;
//   - an internal definition is a member of exactly one bundle, with that
//     bundle's management, and requires nothing a Tenant controls unless
//     it is a fellow member;
//   - requirements exist and have no cycle;
//   - providers agree within a bundle, a requirement covers the clouds of
//     what requires it, and no two definitions install one chart into one
//     namespace on the same cloud (checkProviders);
//   - a role is a Platform definition's, one per cloud (checkRoles).
func CheckCatalog(defs []Definition) []string {
	byName := make(map[string]Definition, len(defs))
	for _, d := range defs {
		byName[d.Name] = d
	}
	bundleOf := map[string][]string{}
	for _, d := range defs {
		for _, m := range d.Includes {
			bundleOf[m] = append(bundleOf[m], d.Name)
		}
	}
	var out []string
	for _, d := range defs {
		out = append(out, checkPlatform(d, byName)...)
		out = append(out, checkBundle(d, byName)...)
		out = append(out, checkInternal(d, byName, bundleOf[d.Name])...)
		out = append(out, checkProviders(d, byName, defs)...)
	}
	out = append(out, checkRoles(defs)...)
	if _, err := release.UpgradeOrder(orderEntries(defs)); err != nil {
		out = append(out, err.Error())
	}
	slices.Sort(out)
	return out
}

func checkPlatform(d Definition, byName map[string]Definition) []string {
	if !d.IsPlatform() {
		return nil
	}
	var out []string
	for _, r := range d.Requires {
		if req, ok := byName[r]; ok && !req.IsPlatform() {
			out = append(out, fmt.Sprintf("%s is a Platform component but requires %s, which a tenant controls", d.Name, r))
		}
	}
	return out
}

func checkBundle(d Definition, byName map[string]Definition) []string {
	if !d.IsBundle() {
		return nil
	}
	var out []string
	for _, name := range d.Includes {
		m, ok := byName[name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s includes %s, which the catalog does not have", d.Name, name))
		case m.IsBundle():
			out = append(out, fmt.Sprintf("%s includes %s, which is a bundle; bundles do not nest", d.Name, name))
		case !m.Internal:
			out = append(out, fmt.Sprintf("%s includes %s, which is not internal; a member cannot also be enabled by itself", d.Name, name))
		case d.IsPlatform() != m.IsPlatform():
			out = append(out, fmt.Sprintf("%s and its member %s differ in management", d.Name, name))
		}
	}
	return out
}

func checkInternal(d Definition, byName map[string]Definition, bundles []string) []string {
	if !d.Internal || d.IsBundle() {
		return nil
	}
	var out []string
	switch len(bundles) {
	case 0:
		out = append(out, fmt.Sprintf("%s is internal but no bundle includes it, so nothing can install it", d.Name))
	case 1:
	default:
		slices.Sort(bundles)
		out = append(out, fmt.Sprintf("%s is a member of more than one bundle: %s", d.Name, strings.Join(bundles, ", ")))
	}
	if len(bundles) != 1 {
		return out
	}
	members := byName[bundles[0]].Includes
	for _, r := range d.Requires {
		req, ok := byName[r]
		if !ok || req.IsPlatform() || slices.Contains(members, r) {
			continue
		}
		out = append(out, fmt.Sprintf("%s requires %s, which a tenant controls and is not a member of %s", d.Name, r, bundles[0]))
	}
	return out
}

// orderEntries are definitions as release.UpgradeOrder takes them; a
// bundle counts as requiring its members, so it comes after them.
func orderEntries(defs []Definition) []release.Definition {
	out := make([]release.Definition, 0, len(defs))
	for _, d := range defs {
		out = append(out, release.Definition{Name: d.Name, Requires: slices.Concat(d.Requires, d.Includes)})
	}
	return out
}

// BundleOrder is the order in which the members of bundle d are installed
// (and upgraded; removal is the reverse): every member after the members
// it requires, then by name. Requirements outside the bundle (a Platform
// component) do not order the members. An error when d is no bundle, a
// member is unknown, is a bundle itself or is not internal, or the
// members require each other in a cycle.
func BundleOrder(d Definition, defs []Definition) ([]string, error) {
	if !d.IsBundle() {
		return nil, fmt.Errorf("component %s is not a bundle", d.Name)
	}
	byName := make(map[string]Definition, len(defs))
	for _, x := range defs {
		byName[x.Name] = x
	}
	entries := make([]release.Definition, 0, len(d.Includes))
	for _, name := range d.Includes {
		m, ok := byName[name]
		switch {
		case !ok:
			return nil, fmt.Errorf("bundle %s includes %s, which the catalog does not have", d.Name, name)
		case m.IsBundle():
			return nil, fmt.Errorf("bundle %s includes %s, which is a bundle; bundles do not nest", d.Name, name)
		case !m.Internal:
			return nil, fmt.Errorf("bundle %s includes %s, which is not internal", d.Name, name)
		}
		var requires []string
		for _, r := range m.Requires {
			if slices.Contains(d.Includes, r) {
				requires = append(requires, r)
			}
		}
		entries = append(entries, release.Definition{Name: name, Requires: requires})
	}
	order, err := release.UpgradeOrder(entries)
	if err != nil {
		return nil, fmt.Errorf("bundle %s: %w", d.Name, err)
	}
	return order, nil
}

// Off are the definitions that are not in use, by name, each with why:
// "disabled" for one the platform team switched off (Disabled), "being
// deleted" for one whose delete was asked for (Deleting), and for
// what cannot work without one that is off, a member of an off bundle and
// whatever requires something that is off. A definition that is on is not
// in the map. The catalog is not changed.
func Off(defs []Definition) map[string]string {
	off := map[string]string{}
	ordered := slices.Clone(defs)
	slices.SortFunc(ordered, func(a, b Definition) int { return strings.Compare(a.Name, b.Name) })
	for _, d := range ordered {
		switch {
		case d.Deleting:
			off[d.Name] = "being deleted"
		case d.Disabled:
			off[d.Name] = "disabled"
		}
	}
	for changed := true; changed; {
		changed = false
		for _, d := range ordered {
			if _, done := off[d.Name]; done {
				continue
			}
			if why := offBecause(d, ordered, off); why != "" {
				off[d.Name], changed = why, true
			}
		}
	}
	return off
}

// offBecause is why d is off given what is off already; "" when it is on.
func offBecause(d Definition, defs []Definition, off map[string]string) string {
	for _, b := range defs {
		if _, bundleOff := off[b.Name]; bundleOff && slices.Contains(b.Includes, d.Name) {
			return fmt.Sprintf("its bundle %s is off", b.Name)
		}
	}
	for _, r := range sortedCopy(d.Requires) {
		if _, requiredOff := off[r]; requiredOff {
			return fmt.Sprintf("it requires %s, which is off", r)
		}
	}
	return ""
}

// PlatformComponents are the definitions the platform installs with every
// cluster, in install order: the Platform definitions that have a chart of
// their own, bundles expanded to their members (a bundle itself installs
// nothing), each after what it requires, then by name. The cluster's
// reconcile renders them after the cluster can be reached. What is off
// (Off) is left out: a cluster that has it installed removes it. An error
// when the Platform definitions are inconsistent (CheckCatalog).
func PlatformComponents(defs []Definition) ([]Definition, error) {
	byName := make(map[string]Definition, len(defs))
	off := Off(defs)
	var entries []release.Definition
	for _, d := range defs {
		byName[d.Name] = d
		if _, isOff := off[d.Name]; isOff || !d.IsPlatform() || d.IsBundle() {
			continue
		}
		entries = append(entries, release.Definition{Name: d.Name, Requires: d.Requires})
	}
	for _, e := range entries {
		for _, r := range e.Requires {
			if req, ok := byName[r]; ok && !req.IsPlatform() {
				return nil, fmt.Errorf("platform component %s requires %s, which a tenant controls", e.Name, r)
			}
		}
	}
	order, err := release.UpgradeOrder(entries)
	if err != nil {
		return nil, err
	}
	out := make([]Definition, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	for _, d := range out {
		for _, r := range d.Requires {
			if req := byName[r]; req.Sequence > d.Sequence {
				return nil, fmt.Errorf("platform component %s (sequence %d) requires %s, which has the later sequence %d", d.Name, d.Sequence, r, req.Sequence)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b Definition) int { return cmp.Compare(a.Sequence, b.Sequence) })
	return out, nil
}

// TenantCatalog are the definitions tenants see: Tenant, not internal, by
// name. A disabled one is listed too (Disabled): it cannot be enabled.
func TenantCatalog(defs []Definition) []Definition {
	var out []Definition
	for _, d := range defs {
		if !d.IsPlatform() && !d.Internal {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b Definition) int { return strings.Compare(a.Name, b.Name) })
	return out
}
