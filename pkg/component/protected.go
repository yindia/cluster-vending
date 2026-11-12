package component

import (
	"slices"
	"strings"
)

// ProtectedPaths are the dotted paths of the tenant values that touch a
// protected key, sorted and without repeats: a value at the key or below
// it (any value, null included), or a value at a parent of the key that is
// not an object and so would replace it. An object at a parent is fine:
// maps merge key by key, and what it holds is looked at in turn.
func ProtectedPaths(tenant Values, protected []string) []string {
	seen := map[string]bool{}
	for _, key := range protected {
		if p, ok := touches(tenant, splitPath(key)); ok {
			seen[p] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// touches reports the tenant path that sets the protected path, if one
// does.
func touches(tenant Values, path []string) (string, bool) {
	if len(path) == 0 {
		return "", false
	}
	cur := tenant
	for i, k := range path {
		v, ok := cur[k]
		if !ok {
			return "", false
		}
		if i == len(path)-1 {
			return strings.Join(path, "."), true
		}
		next, isMap := v.(map[string]any)
		if !isMap {
			return strings.Join(path[:i+1], "."), true
		}
		cur = next
	}
	return "", false
}
