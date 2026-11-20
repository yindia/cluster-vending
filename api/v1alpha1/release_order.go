package v1alpha1

import (
	"cmp"
	"slices"
)

// Release order. A release is newer than another by its spec.serial, not
// by its creation time: a release re-created by a GitOps tool keeps its
// place. A release that predates the field has no serial and is older than
// any release with one; among those, the creation time orders them (a
// release without a time first), then the name (a later name is newer).

// compareReleases orders two releases, negative when a is older than b.
func compareReleases(a, b *Release) int {
	switch {
	case a.Spec.Serial != nil && b.Spec.Serial != nil:
		if c := cmp.Compare(*a.Spec.Serial, *b.Spec.Serial); c != 0 {
			return c
		}
	case a.Spec.Serial != nil:
		return 1
	case b.Spec.Serial != nil:
		return -1
	default:
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
	}
	// Ties: the name, so that the order (and the backfill, which numbers
	// from the oldest) is total and the same for every reader.
	return cmp.Compare(a.Name, b.Name)
}

// SortReleasesNewestFirst sorts releases by serial, newest first.
func SortReleasesNewestFirst(rs []Release) {
	slices.SortStableFunc(rs, func(a, b Release) int { return -compareReleases(&a, &b) })
}

// ReleaseNotNewer reports whether target is known not to be newer than
// current. Releases with a serial are compared by it, and one with a
// serial is newer than one without. Two releases without a serial fall
// back to their creation times, and either one without a time is not
// known (false: it does not hold a move back).
func ReleaseNotNewer(current, target *Release) bool {
	switch {
	case current.Spec.Serial != nil && target.Spec.Serial != nil:
		return *target.Spec.Serial <= *current.Spec.Serial
	case current.Spec.Serial != nil:
		return true
	case target.Spec.Serial != nil:
		return false
	}
	c, t := current.CreationTimestamp, target.CreationTimestamp
	return !c.IsZero() && !t.IsZero() && !t.After(c.Time)
}

// BackfillSerials returns the releases with a serial on every one: those
// that have one keep it, the others get the numbers above the highest
// existing serial, in creation order (no time first, ties by name). The
// input is not changed.
func BackfillSerials(rs []Release) []Release {
	out := make([]Release, len(rs))
	for i := range rs {
		out[i] = *rs[i].DeepCopy()
	}
	next := int64(0)
	var missing []int
	for i := range out {
		if s := out[i].Spec.Serial; s != nil {
			next = max(next, *s)
		} else {
			missing = append(missing, i)
		}
	}
	slices.SortFunc(missing, func(i, j int) int { return compareReleases(&out[i], &out[j]) })
	for _, i := range missing {
		next++
		n := next
		out[i].Spec.Serial = &n
	}
	return out
}
