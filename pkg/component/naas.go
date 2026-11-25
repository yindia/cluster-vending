package component

// ForNaaS selects the platform entries applicable to this cluster. The
// NamespaceTenancy definition (roles.go) remains platform managed, but is
// opt-in through ClusterSpec.naas. Filter before planning release moves as
// well as rendering, so a disabled component neither installs nor holds up
// a release rollout. Every entry playing the role is gated, whatever its
// name or cloud.
//
// Entries are already in dependency order. Dependents of an omitted entry are
// omitted too; they must not be installed without their tenancy prerequisite.
// The input slice and definitions are never changed.
func ForNaaS(entries []PlatformEntry, enabled bool) []PlatformEntry {
	out := make([]PlatformEntry, 0, len(entries))
	excluded := map[string]bool{}
	defs := make([]Definition, 0, len(entries))
	for _, entry := range entries {
		defs = append(defs, entry.Definition)
	}
	for _, entry := range entries {
		off := !enabled && RoleOf(entry.Definition, defs) == RoleNamespaceTenancy
		for _, required := range entry.Requires {
			off = off || excluded[required]
		}
		if off {
			excluded[entry.Name] = true
			continue
		}
		out = append(out, entry)
	}
	return out
}
