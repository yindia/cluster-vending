package component

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yindia/cluster-vending/pkg/fault"
)

// UpgradeMode is the cluster's upgrade mode; empty is Manual.
type UpgradeMode string

// Upgrade modes.
const (
	Manual    UpgradeMode = "Manual"
	Automatic UpgradeMode = "Automatic"
)

// Cluster is what the checks need of the cluster a component goes on.
type Cluster struct {
	// Name, for messages.
	Name string
	// State is the cluster's stored state, for messages (e.g. PROVISIONING).
	State string
	// Running: the cluster is RUNNING.
	Running bool
	// Deleting: the cluster is being deleted.
	Deleting bool
	// Imported: imported and not taken over.
	Imported bool
	Mode     UpgradeMode
	// KubernetesMinor the cluster runs, e.g. "1.37".
	KubernetesMinor string
	// Addons are the names in the cluster's spec.aws.addons.
	Addons []string
	// IRSA: the cluster has its IAM OIDC provider (spec.aws.irsa).
	IRSA bool
}

// Offering is what the release that applies to the cluster offers for its
// Kubernetes minor (pkg/release decides which release that is).
type Offering struct {
	// Release is the release's name.
	Release string
	// Supported: the cluster's minor is in the release's support window.
	Supported bool
	// Versions: component name → the version the release lists for the
	// cluster's minor. A component missing here is not offered.
	Versions map[string]string
	// Pinnable: component name → every version a valid, not withdrawn
	// release lists for the cluster's minor (a manual cluster's pins).
	Pinnable map[string][]string
}

// Request is one enable or update of a component.
type Request struct {
	// Component is the definition name the tenant asked for.
	Component string
	// Definition is the catalog entry; nil when there is none.
	Definition *Definition
	// Cluster is the target; nil when the tenant has no such cluster.
	Cluster *Cluster
	// Enabled are the components already enabled on the cluster (an
	// update's own component included).
	Enabled []string
	// Offering is what the cluster's release offers; nil when no release
	// applies (none is published, or its channel offers none).
	Offering *Offering
	// Catalog are the catalog's definitions, to tell what a requirement is
	// (a Platform component is on every cluster) and which bundle an
	// internal component belongs to. May be nil.
	Catalog []Definition
	// Update: an update of an enabled component, not an enable.
	Update bool
	// Version is a pin; empty: the release's version.
	Version string
	// Values are the tenant's overrides.
	Values Values
}

// Problem is one failed check.
type Problem struct {
	// Check is the check's number in the design (section 4.4, 1-12).
	Check int
	Kind  fault.Kind
	// Message names what to fix.
	Message string
}

// CheckEnable runs the request checks of an enable or update (design
// section 4.4) and returns every problem; none when the request is
// accepted. A missing definition or cluster, or a component already
// enabled, is the only problem reported: nothing else can be judged
// without them.
func CheckEnable(r Request) []Problem {
	if p := checkTargets(r); p != nil {
		return []Problem{*p}
	}
	out := make([]Problem, 0, 4)
	out = append(out, checkCluster(r.Cluster)...)
	out = append(out, checkOffered(r)...)
	out = append(out, checkPin(r)...)
	out = append(out, checkDependencies(r)...)
	out = append(out, checkValues(r.Definition, r.Values)...)
	return out
}

// Err is the error an API answers problems with: one fault listing every
// problem. Its kind is the one problem's kind when there is one; with
// several, Invalid when any is about the request itself (it must change
// first), else FailedPrecondition. Nil without problems.
func Err(problems []Problem) error {
	if len(problems) == 0 {
		return nil
	}
	kind := problems[0].Kind
	if len(problems) > 1 {
		kind = fault.FailedPrecondition
		if slices.ContainsFunc(problems, func(p Problem) bool { return p.Kind == fault.Invalid }) {
			kind = fault.Invalid
		}
	}
	msgs := make([]string, 0, len(problems))
	for _, p := range problems {
		msgs = append(msgs, p.Message)
	}
	return fault.New(kind, strings.Join(msgs, "; "))
}

// checkTargets: checks 1, 2 (existence) and 4.
func checkTargets(r Request) *Problem {
	switch {
	case r.Definition == nil || !r.Definition.Valid:
		return &Problem{Check: 1, Kind: fault.NotFound, Message: fmt.Sprintf("component %s is not in the catalog", r.Component)}
	case CheckTenantAccess(*r.Definition, r.Catalog) != nil:
		// Platform components come with the cluster, and a bundle's
		// members with the bundle: not a tenant's to enable or change.
		return &Problem{Check: 1, Kind: fault.PermissionDenied, Message: CheckTenantAccess(*r.Definition, r.Catalog).Error()}
	case r.Cluster == nil:
		return &Problem{Check: 2, Kind: fault.NotFound, Message: "the tenant has no such cluster"}
	case !r.Update && slices.Contains(r.Enabled, r.Component):
		return &Problem{Check: 4, Kind: fault.AlreadyExists,
			Message: fmt.Sprintf("component %s is already enabled on cluster %s", r.Component, r.Cluster.Name)}
	}
	return nil
}

// checkCluster: checks 2 (state) and 3.
func checkCluster(c *Cluster) []Problem {
	switch {
	case c.Deleting:
		return []Problem{{Check: 2, Kind: fault.FailedPrecondition, Message: fmt.Sprintf("cluster %s is being deleted", c.Name)}}
	case c.Imported:
		return []Problem{{Check: 2, Kind: fault.FailedPrecondition,
			Message: fmt.Sprintf("cluster %s is imported read-only; take the cluster over first", c.Name)}}
	case !c.Running:
		return []Problem{{Check: 3, Kind: fault.FailedPrecondition,
			Message: fmt.Sprintf("the cluster is %s; enable components once it is RUNNING", c.State)}}
	}
	return nil
}

// checkOffered: checks 5 and 12.
func checkOffered(r Request) []Problem {
	o, minor := r.Offering, r.Cluster.KubernetesMinor
	if o == nil {
		return []Problem{{Check: 5, Kind: fault.FailedPrecondition,
			Message: fmt.Sprintf("no release applies to cluster %s, so no component is offered on it", r.Cluster.Name)}}
	}
	var out []Problem
	if !o.Supported {
		out = append(out, Problem{Check: 12, Kind: fault.FailedPrecondition,
			Message: fmt.Sprintf("Kubernetes %s is outside the support window of release %s; upgrade the cluster before enabling components", minor, o.Release)})
	}
	if _, ok := o.Versions[r.Component]; !ok {
		out = append(out, Problem{Check: 5, Kind: fault.FailedPrecondition,
			Message: fmt.Sprintf("%s is not offered on Kubernetes %s in release %s", r.Component, minor, o.Release)})
	}
	return out
}

// checkPin: check 6. Automatic clusters follow the release; a manual
// cluster may pin a version some valid release lists for its minor.
func checkPin(r Request) []Problem {
	if r.Version == "" {
		return nil
	}
	if r.Cluster.Mode == Automatic {
		return []Problem{{Check: 6, Kind: fault.Invalid,
			Message: fmt.Sprintf("cluster %s is automatic: its components follow its release; remove the version", r.Cluster.Name)}}
	}
	var pinnable []string
	if r.Offering != nil {
		pinnable = r.Offering.Pinnable[r.Component]
	}
	if slices.Contains(pinnable, r.Version) {
		return nil
	}
	msg := fmt.Sprintf("version %s of %s is not listed for Kubernetes %s by any release", r.Version, r.Component, r.Cluster.KubernetesMinor)
	if len(pinnable) > 0 {
		sorted := slices.Clone(pinnable)
		slices.Sort(sorted)
		msg += "; listed: " + strings.Join(sorted, ", ")
	}
	return []Problem{{Check: 6, Kind: fault.Invalid, Message: msg}}
}

// checkDependencies: checks 7 and 8.
func checkDependencies(r Request) []Problem {
	var out []Problem
	for _, req := range sortedCopy(r.Definition.Requires) {
		if !slices.Contains(r.Enabled, req) && !platformProvides(req, r.Catalog) {
			out = append(out, Problem{Check: 7, Kind: fault.FailedPrecondition, Message: fmt.Sprintf("enable %s first", req)})
		}
	}
	for _, addon := range sortedCopy(r.Definition.ConflictsWithAddons) {
		if slices.Contains(r.Cluster.Addons, addon) {
			out = append(out, Problem{Check: 8, Kind: fault.FailedPrecondition,
				Message: fmt.Sprintf("remove the %s add-on first: it conflicts with component %s", addon, r.Definition.Name)})
		}
	}
	return out
}

// checkValues: checks 9 (size), 10 (no locked path: a leaf path of
// platformValues) and 11 (valuesSchema). v are the tenant's overrides,
// after PruneUnchanged.
func checkValues(d *Definition, v Values) []Problem {
	if len(v) == 0 {
		return nil
	}
	size, err := valuesSize(v)
	switch {
	case err != nil:
		return []Problem{{Check: 9, Kind: fault.Invalid, Message: err.Error()}}
	case size > MaxValuesSize:
		return []Problem{{Check: 9, Kind: fault.Invalid, Message: fmt.Sprintf("values are %d bytes; at most %d are accepted", size, MaxValuesSize)}}
	}
	var out []Problem
	paths := LockedPathsTouched(v, d.PlatformValues)
	if len(paths) > 0 {
		out = append(out, Problem{Check: 10, Kind: fault.Invalid,
			Message: "only the platform sets " + strings.Join(paths, ", ")})
	}
	if d.ValuesSchema == nil {
		return append(out, Problem{Check: 11, Kind: fault.Invalid, Message: fmt.Sprintf("component %s accepts no values", d.Name)})
	}
	// A refused key is reported once, as locked, not again by the schema
	// (which usually does not list platform keys).
	rest := clone(v)
	for _, p := range paths {
		remove(rest, splitPath(p))
	}
	for _, msg := range ValidateSchema(d.ValuesSchema, rest) {
		out = append(out, Problem{Check: 11, Kind: fault.Invalid, Message: msg})
	}
	return out
}

// CheckDisable refuses disabling a component another enabled component
// requires (design section 8); enabled are the definitions of the
// cluster's enabled components.
func CheckDisable(name string, enabled []Definition) error {
	var needed []string
	for _, d := range enabled {
		if d.Name == name {
			if err := CheckTenantAccess(d, enabled); err != nil {
				return err
			}
		}
		if d.Name != name && slices.Contains(d.Requires, name) {
			needed = append(needed, d.Name)
		}
	}
	if len(needed) == 0 {
		return nil
	}
	slices.Sort(needed)
	return fault.Newf(fault.FailedPrecondition, "%s is required by %s; disable %s first",
		name, strings.Join(needed, ", "), strings.Join(needed, ", "))
}

// CheckAddons refuses add-ons in a cluster's spec.aws.addons that an
// enabled component conflicts with (the reverse of check 8, for
// UpdateCluster); enabled are the definitions of the cluster's enabled
// components.
func CheckAddons(addons []string, enabled []Definition) error {
	var msgs []string
	for _, d := range enabled {
		for _, a := range d.ConflictsWithAddons {
			if slices.Contains(addons, a) {
				msgs = append(msgs, fmt.Sprintf("the %s add-on conflicts with component %s; disable the component first", a, d.Name))
			}
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	slices.Sort(msgs)
	return fault.New(fault.FailedPrecondition, strings.Join(msgs, "; "))
}

// platformProvides reports whether name is a Platform component of the
// catalog: installed with every cluster, so a tenant never enables it.
func platformProvides(name string, catalog []Definition) bool {
	return slices.ContainsFunc(catalog, func(d Definition) bool { return d.Name == name && d.IsPlatform() })
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
