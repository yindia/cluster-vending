package component

import (
	"slices"
	"sort"
	"strings"
)

// LeafPaths are the dotted paths of values' leaves: every value that is not
// a non-empty map (scalars, lists, null and {}). Sorted. The leaf paths of
// a definition's platformValues are the paths locked for tenants.
func LeafPaths(values Values) []string {
	var out []string
	walkLeaves(values, nil, func(path []string) { out = append(out, joinPath(path)) })
	sort.Strings(out)
	return out
}

func walkLeaves(values Values, prefix []string, leaf func([]string)) {
	for key, value := range values {
		path := append(slices.Clone(prefix), key)
		if child, ok := value.(map[string]any); ok && len(child) > 0 {
			walkLeaves(child, path, leaf)
			continue
		}
		leaf(path)
	}
}

// OverlappingPaths are the leaf paths of a that collide with a leaf path
// of b: the same path, or one inside the other (a sets controller while b
// sets controller.resources.requests.cpu, or the reverse). Each is named
// once, sorted.
func OverlappingPaths(a, b Values) []string {
	var bPaths [][]string
	walkLeaves(b, nil, func(path []string) { bPaths = append(bPaths, path) })
	seen := map[string]bool{}
	walkLeaves(a, nil, func(path []string) {
		for _, locked := range bPaths {
			if isPrefix(path, locked) || isPrefix(locked, path) {
				seen[joinPath(path)] = true
				return
			}
		}
	})
	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// isPrefix reports whether p is q or a path above it, segment by segment.
func isPrefix(p, q []string) bool {
	return len(p) <= len(q) && slices.Equal(p, q[:len(p)])
}

// LockedPathsTouched are the paths of a tenant's overrides that touch the
// locked paths (the leaf paths of platformValues): a value at a locked
// path, inside it, or above it (null included, which would remove it). An
// enable or update with any is refused, naming them (check 10): the
// platform's values would win the merge anyway, and refusing says so
// instead of ignoring the tenant silently.
func LockedPathsTouched(overrides, platformValues Values) []string {
	return OverlappingPaths(overrides, platformValues)
}

func joinPath(path []string) string { return strings.Join(path, ".") }
