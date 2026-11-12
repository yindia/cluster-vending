package component

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// parse is a values literal written as JSON.
func parse(t *testing.T, raw string) Values {
	t.Helper()
	if raw == "" {
		return nil
	}
	var v Values
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestMerge(t *testing.T) {
	for _, tt := range []struct {
		name                                string
		chart, userValues, tenant, platform string
		want                                string
	}{
		{"later layers win", `{"a":1,"b":1,"c":1}`, `{"b":2,"c":2}`, `{"a":4}`, `{"c":3}`, `{"a":4,"b":2,"c":3}`},
		{"the tenant wins over the file", ``, `{"replicas":2}`, `{"replicas":3}`, ``, `{"replicas":3}`},
		{"platform values win over the tenant", ``, ``, `{"s":{"k":"tenant","o":1}}`, `{"s":{"k":"platform"}}`,
			`{"s":{"k":"platform","o":1}}`},
		{"maps merge key by key", `{"m":{"x":1,"y":1}}`, `{"m":{"y":2}}`, `{"m":{"z":4}}`, ``, `{"m":{"x":1,"y":2,"z":4}}`},
		{"lists replace", `{"l":[1,2,3]}`, `{"l":[4]}`, `{"l":[5,6]}`, ``, `{"l":[5,6]}`},
		{"null removes a default", `{"a":1,"m":{"x":1,"y":1}}`, ``, `{"a":null,"m":{"x":null}}`, ``, `{"m":{"y":1}}`},
		{"a map replaces a scalar and back", `{"a":1,"b":{"x":1}}`, ``, `{"a":{"x":2},"b":3}`, ``, `{"a":{"x":2},"b":3}`},
		{"nil everywhere", ``, ``, ``, ``, `{}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Merge(parse(t, tt.chart), parse(t, tt.userValues), parse(t, tt.tenant), parse(t, tt.platform))
			if g := asJSON(t, got); g != tt.want {
				t.Errorf("Merge = %s, want %s", g, tt.want)
			}
		})
	}
}

// Merge changes none of its inputs and shares nothing with them.
func TestMergeLeavesItsInputsAlone(t *testing.T) {
	chart, userValues := parse(t, `{"m":{"x":1},"l":[1]}`), parse(t, `{"m":{"y":2}}`)
	tenant, platform := parse(t, `{"m":{"z":3},"a":null}`), parse(t, `{"p":{"k":1}}`)
	before := asJSON(t, []Values{chart, userValues, tenant, platform})
	got := Merge(chart, userValues, tenant, platform)
	got["m"].(map[string]any)["x"] = 99
	got["l"].([]any)[0] = 99
	got["p"].(map[string]any)["k"] = 99
	if after := asJSON(t, []Values{chart, userValues, tenant, platform}); after != before {
		t.Errorf("inputs changed:\n%s\nwas\n%s", after, before)
	}
}

// The Karpenter example of the design (section 6.3) merges into the
// effective values it shows (computed values, a later step, aside).
func TestMergeKarpenterExample(t *testing.T) {
	d := karpenterDefinition()
	userValues, err := ParseUserValues(d.UserValues)
	if err != nil {
		t.Fatal(err)
	}
	got := Merge(nil, userValues, karpenterTenantValues(t), d.PlatformValues)
	want := parse(t, `{
		"replicas": 2, "logLevel": "info",
		"controller": {"resources": {"requests": {"cpu": "250m", "memory": "512Mi"}}},
		"podSecurityContext": {"fsGroup": 65532},
		"settings": {"batchMaxDuration": "10s", "featureGates": {"spotToSpotConsolidation": true}},
		"defaultNodePool": {"enabled": true, "limits": {"cpu": "128"},
			"requirements": [
				{"key": "karpenter.sh/capacity-type", "operator": "In", "values": ["spot", "on-demand"]},
				{"key": "node.kubernetes.io/instance-type", "operator": "In", "values": ["m6i.large", "m6i.xlarge"]}]}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effective values:\n%s\nwant\n%s", asJSON(t, got), asJSON(t, want))
	}
}

func TestLockedPathsTouched(t *testing.T) {
	platform := parse(t, `{"controller":{"resources":{"requests":{"cpu":"250m"}}},"settings":{"isolatedVPC":false},"empty":{}}`)
	for _, tt := range []struct {
		name, tenant string
		want         []string
	}{
		{"nothing locked", `{"replicas":2,"settings":{"batchMaxDuration":"15s"}}`, nil},
		{"at the path", `{"settings":{"isolatedVPC":true}}`, []string{"settings.isolatedVPC"}},
		{"null at the path", `{"settings":{"isolatedVPC":null}}`, []string{"settings.isolatedVPC"}},
		{"below the path", `{"controller":{"resources":{"requests":{"cpu":{"v":1}}}}}`, []string{"controller.resources.requests.cpu.v"}},
		{"a parent that is not an object replaces it", `{"settings":"x"}`, []string{"settings"}},
		{"a null parent replaces it", `{"controller":{"resources":null}}`, []string{"controller.resources"}},
		{"an object parent beside the path is fine", `{"controller":{"resources":{"limits":{"cpu":"1"}}}}`, nil},
		{"an empty locked map is a leaf", `{"empty":{"x":1}}`, []string{"empty.x"}},
		{"every refused path, sorted, once", `{"settings":{"isolatedVPC":1},"controller":5}`, []string{"controller", "settings.isolatedVPC"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := LockedPathsTouched(parse(t, tt.tenant), platform); !reflect.DeepEqual(got, tt.want) && (len(got) != 0 || len(tt.want) != 0) {
				t.Errorf("LockedPathsTouched = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLeafPaths(t *testing.T) {
	got := LeafPaths(parse(t, `{"a":{"b":1,"c":[1]},"d":null,"e":{}}`))
	if want := []string{"a.b", "a.c", "d", "e"}; !reflect.DeepEqual(got, want) {
		t.Errorf("LeafPaths = %v, want %v", got, want)
	}
}

func TestParseValues(t *testing.T) {
	for _, tt := range []struct {
		name, raw, wantErr string
	}{
		{"an object", `{"a":1}`, ""},
		{"empty", ``, ""},
		{"null", `null`, ""},
		{"a list", `[1]`, "must be an object, got array"},
		{"a string", `"x"`, "must be an object, got string"},
		{"not JSON", `{`, "not valid JSON"},
		{"too large", `{"a":"` + strings.Repeat("x", MaxValuesSize) + `"}`, "at most 65536"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseValues([]byte(tt.raw))
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("ParseValues(%.40s) = %v, want %q", tt.raw, err, tt.wantErr)
			}
		})
	}
}
