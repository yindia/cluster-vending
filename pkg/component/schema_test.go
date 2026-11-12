package component

import (
	"reflect"
	"testing"
)

func TestValidateSchema(t *testing.T) {
	schema := parseValues(`{
		"type": "object", "additionalProperties": false, "required": ["name"],
		"properties": {
			"name": {"type": "string", "minLength": 1, "maxLength": 5, "pattern": "^[a-z]+$"},
			"replicas": {"type": "integer", "minimum": 1, "maximum": 3},
			"ratio": {"type": "number"},
			"logLevel": {"type": "string", "enum": ["error", "info", "debug"]},
			"flags": {"type": "object", "additionalProperties": {"type": "boolean"}},
			"list": {"type": "array", "items": {"type": "string"}},
			"either": {"type": ["string", "null"]},
			"free": {"type": "object"}
		}}`)
	for _, tt := range []struct {
		name, values string
		want         []string
	}{
		{"valid", `{"name":"ab","replicas":2,"ratio":0.5,"logLevel":"info","flags":{"x":true},"list":["a"],"either":null,"free":{"anything":[1]}}`, nil},
		{"required", `{}`, []string{"name: is required"}},
		{"unknown key", `{"name":"a","extra":1}`, []string{"extra: is not an accepted value"}},
		{"wrong type", `{"name":1}`, []string{"name: must be string, got integer"}},
		{"integer, not a fraction", `{"name":"a","replicas":1.5}`, []string{"replicas: must be integer, got number"}},
		{"range", `{"name":"a","replicas":9}`, []string{"replicas: must be at most 3"}},
		{"below the minimum", `{"name":"a","replicas":0}`, []string{"replicas: must be at least 1"}},
		{"enum", `{"name":"a","logLevel":"trace"}`, []string{`logLevel: must be one of ["error","info","debug"]`}},
		{"string rules", `{"name":"ABCDEF"}`, []string{"name: must be at most 5 characters", "name: must match ^[a-z]+$"}},
		{"too short", `{"name":""}`, []string{"name: must be at least 1 characters", "name: must match ^[a-z]+$"}},
		{"additionalProperties schema", `{"name":"a","flags":{"x":"yes"}}`, []string{"flags.x: must be boolean, got string"}},
		{"items", `{"name":"a","list":["a",2]}`, []string{"list[1]: must be string, got integer"}},
		{"a list of types", `{"name":"a","either":3}`, []string{"either: must be string or null, got integer"}},
		{"every problem, sorted", `{"replicas":"x","extra":1}`,
			[]string{"extra: is not an accepted value", "name: is required", "replicas: must be integer, got string"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidateSchema(schema, parseValues(tt.values)); !reflect.DeepEqual(got, tt.want) && (len(got) != 0 || len(tt.want) != 0) {
				t.Errorf("ValidateSchema = %q, want %q", got, tt.want)
			}
		})
	}
}

// A key with a dot is quoted in paths, so a path is never ambiguous.
func TestValidateSchemaQuotesDottedKeys(t *testing.T) {
	schema := parseValues(`{"type":"object","properties":{"sel":{"type":"object","additionalProperties":{"type":"string"}}}}`)
	got := ValidateSchema(schema, parseValues(`{"sel":{"karpenter.sh/discovery":1}}`))
	if want := []string{`sel["karpenter.sh/discovery"]: must be string, got integer`}; !reflect.DeepEqual(got, want) {
		t.Errorf("ValidateSchema = %q, want %q", got, want)
	}
}

func TestCheckSchema(t *testing.T) {
	for _, tt := range []struct {
		name, schema string
		want         []string
	}{
		{"the Karpenter schema is usable", asJSONString(karpenterDefinition().ValuesSchema), nil},
		{"an unknown keyword", `{"type":"object","oneOf":[]}`, []string{`schema: keyword "oneOf" is not supported`}},
		{"nested", `{"properties":{"a":{"type":"thing","format":"x"}}}`,
			[]string{`schema/properties/a: keyword "format" is not supported`, `schema/properties/a: unknown type "thing"`}},
		{"a bad pattern", `{"pattern":"("}`, []string{"schema: pattern \"(\": error parsing regexp: missing closing ): `(`"}},
		{"a property that is not a schema", `{"properties":{"a":1}}`, []string{"schema/properties/a: a schema must be an object"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := CheckSchema(parseValues(tt.schema)); !reflect.DeepEqual(got, tt.want) && (len(got) != 0 || len(tt.want) != 0) {
				t.Errorf("CheckSchema = %q, want %q", got, tt.want)
			}
		})
	}
}
