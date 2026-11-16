package component

// ForNaaS selects the platform entries applicable to this cluster. Capsule
// remains platform managed, but is opt-in through ClusterSpec.naas. Filter
// before planning release moves as well as rendering, so a disabled component
// neither installs nor holds up a release rollout. Chart matching also covers
// definitions named for another provider, such as capsule-gcp.
//
// Entries are already in dependency order. Dependents of an omitted entry are
// omitted too; they must not be installed without their Capsule prerequisite.
// The input slice and definitions are never changed.
func ForNaaS(entries []PlatformEntry, enabled bool) []PlatformEntry {
	out := make([]PlatformEntry, 0, len(entries))
	excluded := map[string]bool{}
	for _, entry := range entries {
		off := !enabled && (entry.Name == "capsule" || entry.Install.Main.Name == "capsule")
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
