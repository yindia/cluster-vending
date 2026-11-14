package component

import (
	"fmt"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// TenantEntries are the catalog's tenant components a tenant may enable
// (not Platform, not internal, not disabled by the platform team, with an
// install of their own), each with its install. A definition that is not usable (CheckDefinition) is left
// out: a component of it reports that it is not in the catalog.
func TenantEntries(defs []cloudv1alpha1.ComponentDefinition) []PlatformEntry {
	var out []PlatformEntry
	for i := range defs {
		d, err := DefinitionOf(&defs[i])
		if err != nil || len(CheckDefinition(d)) > 0 || d.IsPlatform() || d.Internal || d.Disabled {
			continue
		}
		install, ok := InstallOf(&defs[i])
		if !ok {
			continue
		}
		out = append(out, PlatformEntry{Definition: d, Install: install})
	}
	return out
}

// LastRelease is the Helm release a component's install ends with: its
// last companion the release pins, else its main chart. What requires
// the component waits until that release is deployed.
func LastRelease(e PlatformEntry, pins map[string]Pin) string {
	last := e.Name
	for _, c := range e.Install.Companions {
		if _, ok := pins[c.Name]; ok {
			last = c.Name
		}
	}
	return last
}

// PlanTenant resolves one tenant component of a cluster as PlanPlatform
// resolves a Platform one (version and locked values from the release
// pin, the definition's platformValues with their placeholders filled in,
// the IAM role), and merges the values in the order of Merge: the
// definition's userValues file, then the tenant's stored values, then the
// platform's, which always win. The tenant's values and the file are
// taken as they are: a placeholder in them is not filled in.
//
// ready names, for each component the tenant component may require (one
// the platform installs, or another tenant component), the last Release
// object of its install (LastRelease, as an object name); a required
// component missing from it is not installed, and the tenant component is
// left out with the reason. A required component in ready is waited for
// (PlatformPlan.External). reason is "" when the component is planned.
// No input is changed.
func PlanTenant(in PlatformInput, e PlatformEntry, tenant Values, ready map[string]string) (plan crossplaneaws.PlatformPlan, reason string) {
	plan = in.Plan
	plan.Components, plan.External = nil, map[string]string{}
	if !e.Covers(in.Cluster.Provider) {
		return plan, NotOnProvider(e.Definition, in.Cluster.Provider)
	}
	for _, r := range e.Requires {
		object, ok := ready[r]
		if !ok {
			return plan, fmt.Sprintf("requires %s, which is not installed", r)
		}
		plan.External[r] = object
	}
	c, reason := planComponent(in, e, map[string]bool{})
	if reason != "" {
		return plan, reason
	}
	file, err := ParseUserValues(e.UserValues)
	if err != nil {
		return plan, "its values file: " + err.Error()
	}
	layer := func(v Values, key string) Values {
		if key == "" {
			out := clone(v)
			for _, comp := range e.Install.Companions {
				delete(out, comp.Name)
			}
			return out
		}
		own, _ := v[key].(map[string]any)
		return clone(own)
	}
	c.Main.Values = Merge(nil, layer(file, ""), layer(tenant, ""), c.Main.Values)
	for i := range c.Companions {
		name := c.Companions[i].Name
		c.Companions[i].Values = Merge(nil, layer(file, name), layer(tenant, name), c.Companions[i].Values)
	}
	plan.Components = []crossplaneaws.PlatformComponentPlan{c}
	return plan, ""
}
