package component

import (
	"slices"
	"strings"
)

// Merge returns the effective values of a component (design section 5),
// later layers winning: the chart's defaults, the definition's userValues
// file, the tenant's overrides, then the definition's platformValues
// (computed values, a later step, will go last). Maps merge key by key,
// everything else (lists included) replaces, and an explicit null removes
// what the layers below set. platformValues always win; that a tenant's
// overrides never touch them is checked before (LockedPathsTouched, check
// 10), so the merge never overrides a tenant silently through the API. No
// input is modified; the result shares nothing with them.
func Merge(chart, userValues, tenant, platformValues Values) Values {
	out := Values{}
	for _, layer := range []Values{chart, userValues, tenant, platformValues} {
		out = mergeInto(out, layer)
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

// splitPath is a dotted path's segments, without empty ones.
func splitPath(key string) []string {
	return slices.DeleteFunc(strings.Split(key, "."), func(s string) bool { return s == "" })
}

// remove deletes the value at path, if there is one, and the maps above it
// that are left empty: what is left is what the tenant set besides it.
func remove(v Values, path []string) {
	if len(path) == 0 {
		return
	}
	if len(path) == 1 {
		delete(v, path[0])
		return
	}
	next, ok := v[path[0]].(map[string]any)
	if !ok {
		return
	}
	remove(next, path[1:])
	if len(next) == 0 {
		delete(v, path[0])
	}
}
