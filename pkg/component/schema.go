package component

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// The values schema is a JSON Schema subset, the part the design needs:
// type (a name or a list of names: object, array, string, integer,
// number, boolean, null), properties, required, additionalProperties (a
// boolean or a schema), items, enum, minimum, maximum, minLength,
// maxLength, pattern, minItems, maxItems, minProperties and maxProperties,
// plus description and title (ignored). No library
// in the module graph validates plain JSON Schema; the Kubernetes ones
// would bring Kubernetes into this package. CheckSchema refuses any other
// keyword, so an operator never writes a rule that is silently ignored.

// schemaKeywords are the keywords the subset knows.
var schemaKeywords = []string{
	"type", "properties", "required", "additionalProperties", "items", "enum",
	"minimum", "maximum", "minLength", "maxLength", "pattern", "description", "title",
	"minItems", "maxItems", "minProperties", "maxProperties",
}

// schemaTypes are the type names the subset knows.
var schemaTypes = []string{"object", "array", "string", "integer", "number", "boolean", "null"}

// CheckSchema reports what in a definition's values schema the subset does
// not support or cannot read (for the definition's Valid condition), every
// problem at once; nil when it is usable.
func CheckSchema(schema Values) []string {
	var out []string
	checkSchemaAt(schema, "", &out)
	sort.Strings(out)
	return out
}

func checkSchemaAt(s Values, at string, out *[]string) {
	where := schemaWhere(at)
	for k := range s {
		if !slices.Contains(schemaKeywords, k) {
			*out = append(*out, fmt.Sprintf("%s: keyword %q is not supported", where, k))
		}
	}
	for _, t := range typeNames(s["type"]) {
		if !slices.Contains(schemaTypes, t) {
			*out = append(*out, fmt.Sprintf("%s: unknown type %q", where, t))
		}
	}
	if p, ok := s["pattern"].(string); ok {
		if _, err := regexp.Compile(p); err != nil {
			*out = append(*out, fmt.Sprintf("%s: pattern %q: %v", where, p, err))
		}
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for name, sub := range props {
			checkSubSchema(sub, at+"/properties/"+name, out)
		}
	}
	if add, ok := s["additionalProperties"].(map[string]any); ok {
		checkSchemaAt(add, at+"/additionalProperties", out)
	}
	if items, ok := s["items"]; ok {
		checkSubSchema(items, at+"/items", out)
	}
}

func checkSubSchema(sub any, at string, out *[]string) {
	m, ok := sub.(map[string]any)
	if !ok {
		*out = append(*out, fmt.Sprintf("%s: a schema must be an object", schemaWhere(at)))
		return
	}
	checkSchemaAt(m, at, out)
}

func schemaWhere(at string) string {
	if at == "" {
		return "schema"
	}
	return "schema" + at
}

// ValidateSchema checks tenant values against a values schema and returns
// every violation, each with the path of the value, sorted.
func ValidateSchema(schema, values Values) []string {
	var out []string
	validateAt(schema, values, "", &out)
	sort.Strings(out)
	return out
}

// validateAt checks one value against one schema.
func validateAt(s map[string]any, v any, path string, out *[]string) {
	if types := typeNames(s["type"]); len(types) > 0 && !slices.ContainsFunc(types, func(t string) bool { return isType(v, t) }) {
		*out = append(*out, fmt.Sprintf("%s: must be %s, got %s", display(path), strings.Join(types, " or "), typeName(v)))
		return
	}
	if enum, ok := s["enum"].([]any); ok && !slices.ContainsFunc(enum, func(e any) bool { return equalJSON(e, v) }) {
		*out = append(*out, fmt.Sprintf("%s: must be one of %s", display(path), compact(enum)))
	}
	switch t := v.(type) {
	case map[string]any:
		validateObject(s, t, path, out)
	case []any:
		checkCount(s, len(t), "minItems", "maxItems", "items", path, out)
		if items, ok := s["items"].(map[string]any); ok {
			for i, e := range t {
				validateAt(items, e, fmt.Sprintf("%s[%d]", path, i), out)
			}
		}
	case string:
		validateString(s, t, path, out)
	default:
		if n, ok := number(v); ok {
			validateNumber(s, n, path, out)
		}
	}
}

// validateObject checks the property count, required, properties and
// additionalProperties.
func validateObject(s map[string]any, obj map[string]any, path string, out *[]string) {
	checkCount(s, len(obj), "minProperties", "maxProperties", "properties", path, out)
	if req, ok := s["required"].([]any); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				if _, has := obj[name]; !has {
					*out = append(*out, fmt.Sprintf("%s: is required", display(join(path, name))))
				}
			}
		}
	}
	props, _ := s["properties"].(map[string]any)
	for name, v := range obj {
		if sub, ok := props[name].(map[string]any); ok {
			validateAt(sub, v, join(path, name), out)
			continue
		}
		switch add := s["additionalProperties"].(type) {
		case bool:
			if !add {
				*out = append(*out, fmt.Sprintf("%s: is not an accepted value", display(join(path, name))))
			}
		case map[string]any:
			validateAt(add, v, join(path, name), out)
		}
	}
}

func validateString(s map[string]any, v, path string, out *[]string) {
	length := len([]rune(v))
	if n, ok := number(s["maxLength"]); ok && float64(length) > n {
		*out = append(*out, fmt.Sprintf("%s: must be at most %v characters", display(path), n))
	}
	if n, ok := number(s["minLength"]); ok && float64(length) < n {
		*out = append(*out, fmt.Sprintf("%s: must be at least %v characters", display(path), n))
	}
	if p, ok := s["pattern"].(string); ok {
		if re, err := regexp.Compile(p); err == nil && !re.MatchString(v) {
			*out = append(*out, fmt.Sprintf("%s: must match %s", display(path), p))
		}
	}
}

func validateNumber(s map[string]any, v float64, path string, out *[]string) {
	if n, ok := number(s["minimum"]); ok && v < n {
		*out = append(*out, fmt.Sprintf("%s: must be at least %v", display(path), n))
	}
	if n, ok := number(s["maximum"]); ok && v > n {
		*out = append(*out, fmt.Sprintf("%s: must be at most %v", display(path), n))
	}
}

// typeNames reads "type": one name or a list of names.
func typeNames(t any) []string {
	switch x := t.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// isType reports whether v is of the JSON Schema type t.
func isType(v any, t string) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := number(v)
		return ok
	case "integer":
		n, ok := number(v)
		return ok && n == math.Trunc(n)
	}
	return false
}

// number reads a JSON number in any of the forms values arrive in.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// typeName is v's JSON type, for messages.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	if n, ok := number(v); ok {
		if n == math.Trunc(n) {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

// equalJSON compares two JSON values by their encoding.
func equalJSON(a, b any) bool {
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ra, rb)
}

func compact(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}

// join appends a key to a value path; a key with a dot is quoted.
func join(path, key string) string {
	if strings.ContainsAny(key, `.[]"`) {
		return fmt.Sprintf("%s[%q]", path, key)
	}
	if path == "" {
		return key
	}
	return path + "." + key
}

// display is a value path for messages; the root is "values".
func display(path string) string {
	if path == "" {
		return "values"
	}
	return path
}

// checkCount checks a list's or an object's size against the schema's
// minimum and maximum keywords (minKey, maxKey); what names the counted
// things in the message.
func checkCount(s map[string]any, n int, minKey, maxKey, what, path string, out *[]string) {
	if limit, ok := number(s[maxKey]); ok && float64(n) > limit {
		*out = append(*out, fmt.Sprintf("%s: must have at most %v %s", display(path), limit, what))
	}
	if limit, ok := number(s[minKey]); ok && float64(n) < limit {
		*out = append(*out, fmt.Sprintf("%s: must have at least %v %s", display(path), limit, what))
	}
}
