package component

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// Chart is where a chart comes from (the kind's HelmChartSource).
type Chart struct {
	Repository, Name string
	// Private: the repository needs credentials.
	Private                          bool
	InsecureSkipTLSVerify, PlainHTTP bool
}

// Companion is a chart installed after a component's main chart, as the
// Helm release Name.
type Companion struct {
	Name string
	Chart
}

// Install is how a definition reaches a workload cluster
// (spec.install.helm) and where (spec.namespace).
type Install struct {
	Namespace  string
	Main       Chart
	CRDs       *Chart
	Companions []Companion
}

// InstallOf is the install of def; false for a bundle, which has none.
func InstallOf(def *cloudv1alpha1.ComponentDefinition) (Install, bool) {
	if def.Spec.Install == nil || def.Spec.Install.Helm == nil {
		return Install{}, false
	}
	h := def.Spec.Install.Helm
	chart := func(s cloudv1alpha1.HelmChartSource) Chart {
		return Chart{Repository: s.Repository, Name: s.Chart, Private: s.Credentials != nil,
			InsecureSkipTLSVerify: s.InsecureSkipTLSVerify, PlainHTTP: s.PlainHTTP}
	}
	out := Install{Namespace: def.Spec.Namespace, Main: chart(h.HelmChartSource)}
	if h.CRDs != nil {
		crds := chart(*h.CRDs)
		out.CRDs = &crds
	}
	for _, c := range h.Companions {
		out.Companions = append(out.Companions, Companion{Name: c.Name, Chart: chart(c.HelmChartSource)})
	}
	return out, true
}

// PlatformEntry is a Platform definition with its install.
type PlatformEntry struct {
	Definition
	Install Install
}

// PlatformEntries are the catalog's Platform definitions in install order
// (PlatformComponents), each with its install. An invalid Platform definition
// fails planning instead of silently removing an installed component. Invalid
// tenant definitions are excluded. Inconsistent Platform definitions also fail.
func PlatformEntries(defs []cloudv1alpha1.ComponentDefinition) ([]PlatformEntry, error) {
	installs := map[string]Install{}
	var usable []Definition
	for i := range defs {
		d, err := DefinitionOf(&defs[i])
		if err != nil {
			if defs[i].Spec.Management == cloudv1alpha1.ComponentManagementPlatform {
				return nil, fmt.Errorf("platform component %s is invalid: %w", defs[i].Name, err)
			}
			continue
		}
		if problems := CheckDefinition(d); len(problems) > 0 {
			if d.IsPlatform() {
				return nil, fmt.Errorf("platform component %s is invalid: %s", d.Name, strings.Join(problems, "; "))
			}
			continue
		}
		usable = append(usable, d)
		if install, ok := InstallOf(&defs[i]); ok {
			installs[d.Name] = install
		}
	}
	ordered, err := PlatformComponents(usable)
	if err != nil {
		return nil, err
	}
	out := make([]PlatformEntry, 0, len(ordered))
	for _, d := range ordered {
		out = append(out, PlatformEntry{Definition: d, Install: installs[d.Name]})
	}
	return out, nil
}

// Pin is what a release pins for a component on the cluster's Kubernetes
// minor: the chart version and the values it sets (ComponentPin).
type Pin struct {
	Version string
	Values  Values
}

// PlatformInput is everything PlanPlatform resolves the Platform
// components of one cluster from.
type PlatformInput struct {
	// Entries are the Platform definitions in install order
	// (PlatformComponents), each with its install.
	Entries []PlatformEntry
	// Release names the release Pins are of, for messages.
	Release string
	// Pins are the release's pins on the cluster's Kubernetes minor, by
	// component (and companion) name.
	Pins map[string]Pin
	// Cluster is the cluster, as far as the checks need it.
	Cluster Cluster
	// Vars are a component's placeholder values (VarsFor with its IAM
	// outputs).
	Vars func(component string) Vars
	// Plan is the cluster's identity (namespace, IDs, prefix, node group);
	// its Components are filled in.
	Plan crossplaneaws.PlatformPlan
	// IAM is the cluster's part of every component's IAM plan (namespace,
	// ProviderConfig, IDs, names, region, tags).
	IAM crossplaneaws.ComponentIAMPlan
}

// NotInstalled is a Platform component the plan leaves out, and why.
type NotInstalled struct {
	Name, Reason string
	// Preserve prevents render/validation failures from becoming uninstall intent.
	// An absent release pin (and dependents of that explicit removal) is intentional.
	Preserve bool
}

// PlanPlatform resolves the Platform components of a cluster to what is
// rendered: versions from the release's pins, values merged (the pin's
// over the definition's platformValues) with their placeholders filled in,
// the companions' values (the top-level key named after each) split off,
// the IAM role's policy expanded. A component is left out, with the
// reason, when:
//
//   - the release does not pin it;
//   - a chart of it is in a private repository (not supported yet);
//   - its values or policy cannot be expanded (an unknown placeholder);
//   - it uses IRSA and the cluster has it off;
//   - a component it requires is left out.
//
// A companion the release does not pin is left out alone. A component
// whose placeholders have no value yet stays in, waiting
// (PlatformComponentPlan.WaitsFor). No input is changed.
func PlanPlatform(in PlatformInput) (crossplaneaws.PlatformPlan, []NotInstalled) {
	plan := in.Plan
	plan.Components = nil
	var skipped []NotInstalled
	out := map[string]bool{}
	preserve := map[string]bool{}
	for _, e := range in.Entries {
		c, reason := planComponent(in, e, out)
		if reason != "" {
			_, pinned := in.Pins[e.Name]
			keep := pinned
			for _, dependency := range e.Requires {
				if out[dependency] {
					keep = pinned && preserve[dependency]
					break
				}
			}
			out[e.Name], preserve[e.Name] = true, keep
			skipped = append(skipped, NotInstalled{Name: e.Name, Reason: reason, Preserve: keep})
			continue
		}
		plan.Components = append(plan.Components, c)
	}
	return plan, skipped
}

// planComponent is one entry's plan, or why it is left out.
func planComponent(in PlatformInput, e PlatformEntry, out map[string]bool) (crossplaneaws.PlatformComponentPlan, string) {
	none := crossplaneaws.PlatformComponentPlan{}
	pin, pinned := in.Pins[e.Name]
	if !pinned {
		return none, fmt.Sprintf("not in release %s", in.Release)
	}
	for _, r := range e.Requires {
		if out[r] {
			return none, fmt.Sprintf("requires %s, which is not installed", r)
		}
	}
	charts := []Chart{e.Install.Main}
	if e.Install.CRDs != nil {
		charts = append(charts, *e.Install.CRDs)
	}
	for _, c := range e.Install.Companions {
		charts = append(charts, c.Chart)
	}
	if slices.ContainsFunc(charts, func(c Chart) bool { return c.Private }) {
		return none, "its chart repository needs credentials, which platform components do not support yet"
	}
	if err := CheckCluster(e.Name, e.CloudResources, in.Cluster); err != nil {
		return none, err.Error()
	}
	vars := in.Vars(e.Name)
	c := crossplaneaws.PlatformComponentPlan{Name: e.Name, Namespace: e.Install.Namespace, Requires: slices.Clone(e.Requires)}
	var waits []string
	wait := func(err error) bool {
		var notReady *NotReadyError
		if errors.As(err, &notReady) {
			waits = append(waits, notReady.Missing...)
			return true
		}
		return false
	}
	iam, err := planIAM(in.IAM, e, vars)
	if err != nil && !wait(err) {
		return none, err.Error()
	}
	c.IAM = iam
	values := Values{}
	expanded, err := Expand(WithPinValues(e.Definition, pin.Values).PlatformValues, vars)
	switch {
	case err == nil:
		if v, ok := expanded.(map[string]any); ok {
			values = v
		}
	case !wait(err):
		return none, "its values: " + err.Error()
	}
	chart := func(ch Chart, version string) crossplaneaws.HelmChart {
		return crossplaneaws.HelmChart{Repository: ch.Repository, Name: ch.Name, Version: version,
			InsecureSkipTLSVerify: ch.InsecureSkipTLSVerify, PlainHTTP: ch.PlainHTTP}
	}
	for _, comp := range e.Install.Companions {
		own, _ := values[comp.Name].(map[string]any)
		delete(values, comp.Name)
		if p, ok := in.Pins[comp.Name]; ok {
			c.Companions = append(c.Companions, crossplaneaws.HelmReleasePlan{Name: comp.Name, Chart: chart(comp.Chart, p.Version), Values: own})
		}
	}
	c.Main = crossplaneaws.HelmReleasePlan{Name: e.Name, Chart: chart(e.Install.Main, pin.Version), Values: values, SkipCRDs: e.Install.CRDs != nil}
	if e.Install.CRDs != nil {
		c.CRDs = &crossplaneaws.HelmReleasePlan{Name: crossplaneaws.CRDsReleaseName(e.Name), Chart: chart(*e.Install.CRDs, pin.Version)}
	}
	if len(waits) > 0 {
		slices.Sort(waits)
		c.WaitsFor = strings.Join(slices.Compact(waits), ", ")
	}
	return c, ""
}

// planIAM is the component's IAM plan with its policy and managed policy
// ARNs expanded; nil for a component without a role. A *NotReadyError
// when a placeholder has no value yet: the role is then not rendered.
func planIAM(base crossplaneaws.ComponentIAMPlan, e PlatformEntry, vars Vars) (*crossplaneaws.ComponentIAMPlan, error) {
	role := e.CloudResources.IAMRole
	if role == nil {
		return nil, nil
	}
	p := base
	p.Component = e.Name
	p.Access = crossplaneaws.ComponentAccessIRSA
	if !role.UsesIRSA() {
		p.Access = crossplaneaws.ComponentAccessPodIdentity
	}
	for _, sa := range role.ServiceAccounts {
		p.ServiceAccounts = append(p.ServiceAccounts, crossplaneaws.ServiceAccountPlan{Namespace: sa.Namespace, Name: sa.Name})
	}
	if role.Policy != "" {
		policy, err := ExpandPolicy(role.Policy, vars)
		if err != nil {
			return nil, err
		}
		p.Policy = policy
	}
	for _, arn := range role.ManagedPolicyARNs {
		expanded, err := ExpandString(arn, vars)
		if err != nil {
			return nil, err
		}
		p.ManagedPolicyARNs = append(p.ManagedPolicyARNs, expanded)
	}
	return &p, nil
}

// InstalledComponent is one component on a cluster as a tenant sees it in
// the list of installed components.
type InstalledComponent struct {
	// ID is the Component's ID; empty for a Platform component, which has
	// no Component object.
	ID string
	// Name is the catalog name; Version what runs or is being installed.
	Name, Version string
	// Phase and Message are the contract's (ComponentPhase name).
	Phase, Message string
	// Management says who controls it: a Platform entry is read-only.
	Management Management
}

// Installed is what is installed on a cluster, for the list a tenant
// reads: the Platform components the cluster reports
// (ClusterStatus.platform_components), read-only, and the tenant's own
// components, by name. A tenant component with a Platform component's name
// cannot exist (CheckTenantAccess) and is left out. No input is changed.
func Installed(platform, tenant []InstalledComponent) []InstalledComponent {
	out := make([]InstalledComponent, 0, len(platform)+len(tenant))
	names := map[string]bool{}
	for _, c := range platform {
		c.ID, c.Management = "", Platform
		names[c.Name] = true
		out = append(out, c)
	}
	for _, c := range tenant {
		if names[c.Name] {
			continue
		}
		c.Management = Tenant
		out = append(out, c)
	}
	slices.SortStableFunc(out, func(a, b InstalledComponent) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// MissingCompanions are the companions of e the release does not pin.
func MissingCompanions(e PlatformEntry, pins map[string]Pin) []string {
	var out []string
	for _, c := range e.Install.Companions {
		if _, ok := pins[c.Name]; !ok {
			out = append(out, c.Name)
		}
	}
	return out
}
