package component

import (
	"slices"
	"strings"
)

// Merge returns the effective values of a component (design section 5),
// later layers winning: the chart's defaults, the definition's defaults,
// the platform-computed values, then the tenant's values. Maps merge key
// by key, everything else (lists included) replaces, and an explicit null
// removes what the layers below set. Every protected key then has the
// value the first three layers give it, or none: a tenant value there
// never survives, even one a stored object somehow carries. No input is
// modified; the result shares nothing with them.
func Merge(chart, defaults, platform, tenant Values, protected []string) Values {
	base := mergeInto(mergeInto(mergeInto(Values{}, chart), defaults), platform)
	out := mergeInto(clone(base), tenant)
	for _, key := range protected {
		path := splitPath(key)
		if v, ok := lookup(base, path); ok {
			set(out, path, deepCopy(v))
		} else {
			remove(out, path)
		}
	}
	return out
}

// mergeInto merges src into dst (dst is changed and returned).
func mergeInto(dst, src Values) Values {
	for k, v := range src {
		switch sv := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			if dv, ok := dst[k].(map[string]any); ok {
				dst[k] = mergeInto(dv, sv)
				continue
			}
			dst[k] = mergeInto(Values{}, sv)
		default:
			dst[k] = deepCopy(v)
		}
	}
	return dst
}

// clone is a deep copy of v.
func clone(v Values) Values {
	if v == nil {
		return Values{}
	}
	return deepCopy(v).(map[string]any)
}

// deepCopy copies maps and lists, so results never alias their inputs.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}

// splitPath is a protected key's path: dotted, without empty segments.
func splitPath(key string) []string {
	return slices.DeleteFunc(strings.Split(key, "."), func(s string) bool { return s == "" })
}

// lookup is the value at path, and whether there is one.
func lookup(v Values, path []string) (any, bool) {
	var cur any = v
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[k]; !ok {
			return nil, false
		}
	}
	return cur, len(path) > 0
}

// set puts value at path, creating maps on the way (and replacing what is
// not a map there).
func set(v Values, path []string, value any) {
	cur := v
	for _, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = Values{}
			cur[k] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = value
}

// remove deletes the value at path, if there is one.
func remove(v Values, path []string) {
	cur := v
	for _, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
	delete(cur, path[len(path)-1])
}
