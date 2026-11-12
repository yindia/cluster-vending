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
		name                              string
		chart, defaults, platform, tenant string
		protected                         []string
		want                              string
	}{
		{"later layers win", `{"a":1,"b":1}`, `{"b":2,"c":2}`, `{"c":3}`, `{"a":4}`, nil, `{"a":4,"b":2,"c":3}`},
		{"maps merge key by key", `{"m":{"x":1,"y":1}}`, `{"m":{"y":2}}`, ``, `{"m":{"z":4}}`, nil, `{"m":{"x":1,"y":2,"z":4}}`},
		{"lists replace", `{"l":[1,2,3]}`, `{"l":[4]}`, ``, `{"l":[5,6]}`, nil, `{"l":[5,6]}`},
		{"null removes a default", `{"a":1,"m":{"x":1,"y":1}}`, ``, ``, `{"a":null,"m":{"x":null}}`, nil, `{"m":{"y":1}}`},
		{"a map replaces a scalar and back", `{"a":1,"b":{"x":1}}`, ``, ``, `{"a":{"x":2},"b":3}`, nil, `{"a":{"x":2},"b":3}`},
		{"a protected key keeps the platform value", ``, ``, `{"settings":{"clusterName":"prod"}}`,
			`{"settings":{"clusterName":"evil","other":1}}`, []string{"settings.clusterName"}, `{"settings":{"clusterName":"prod","other":1}}`},
		{"a protected key the platform does not set keeps the defaults' value", ``, `{"s":{"k":"def"}}`, ``,
			`{"s":{"k":"tenant"}}`, []string{"s.k"}, `{"s":{"k":"def"}}`},
		{"a protected key nobody below sets is removed", ``, ``, ``, `{"s":{"k":"tenant","o":1}}`, []string{"s.k"}, `{"s":{"o":1}}`},
		{"a parent replaced by the tenant gets the protected key back", ``, ``, `{"s":{"k":"p"}}`, `{"s":5}`, []string{"s.k"}, `{"s":{"k":"p"}}`},
		{"nil everywhere", ``, ``, ``, ``, nil, `{}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Merge(parse(t, tt.chart), parse(t, tt.defaults), parse(t, tt.platform), parse(t, tt.tenant), tt.protected)
			if g := asJSON(t, got); g != tt.want {
				t.Errorf("Merge = %s, want %s", g, tt.want)
			}
		})
	}
}

// Merge changes none of its inputs and shares nothing with them.
func TestMergeLeavesItsInputsAlone(t *testing.T) {
	chart, defaults := parse(t, `{"m":{"x":1},"l":[1]}`), parse(t, `{"m":{"y":2}}`)
	platform, tenant := parse(t, `{"p":{"k":1}}`), parse(t, `{"m":{"z":3},"a":null}`)
	before := asJSON(t, []Values{chart, defaults, platform, tenant})
	got := Merge(chart, defaults, platform, tenant, []string{"p.k"})
	got["m"].(map[string]any)["x"] = 99
	got["l"].([]any)[0] = 99
	got["p"].(map[string]any)["k"] = 99
	if after := asJSON(t, []Values{chart, defaults, platform, tenant}); after != before {
		t.Errorf("inputs changed:\n%s\nwas\n%s", after, before)
	}
}

// The Karpenter example of the design (section 6.3) merges into the
// effective values it shows.
func TestMergeKarpenterExample(t *testing.T) {
	d := karpenterDefinition()
	platform := parse(t, `{
		"settings": {"clusterName": "prod-eks", "clusterEndpoint": "https://eks", "interruptionQueue": "prod-eks-8f819a1f-karpenter"},
		"serviceAccount": {"name": "karpenter"},
		"defaultNodeClass": {"role": "prod-eks-8f819a1f-karpenter-node",
			"subnetSelector": {"karpenter.sh/discovery": "prod-eks"},
			"securityGroupSelector": {"aws:eks:cluster-name": "prod-eks"}}}`)
	got := Merge(nil, d.Defaults, platform, karpenterTenantValues(t), d.ProtectedKeys)
	want := parse(t, `{
		"replicas": 2, "logLevel": "info",
		"controller": {"resources": {"requests": {"cpu": "250m", "memory": "512Mi"}}},
		"settings": {"clusterName": "prod-eks", "clusterEndpoint": "https://eks",
			"interruptionQueue": "prod-eks-8f819a1f-karpenter", "featureGates": {"spotToSpotConsolidation": true}},
		"serviceAccount": {"name": "karpenter"},
		"defaultNodeClass": {"role": "prod-eks-8f819a1f-karpenter-node",
			"subnetSelector": {"karpenter.sh/discovery": "prod-eks"},
			"securityGroupSelector": {"aws:eks:cluster-name": "prod-eks"}},
		"defaultNodePool": {"enabled": true, "limits": {"cpu": "128"},
			"requirements": [
				{"key": "karpenter.sh/capacity-type", "operator": "In", "values": ["spot", "on-demand"]},
				{"key": "node.kubernetes.io/instance-type", "operator": "In", "values": ["m6i.large", "m6i.xlarge"]}]}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effective values:\n%s\nwant\n%s", asJSON(t, got), asJSON(t, want))
	}
}

func TestProtectedPaths(t *testing.T) {
	protected := []string{"settings.clusterName", "settings.interruptionQueue", "serviceAccount.annotations", "defaultNodeClass"}
	for _, tt := range []struct {
		name, tenant string
		want         []string
	}{
		{"nothing protected", `{"replicas":2,"settings":{"featureGates":{}}}`, nil},
		{"at the key", `{"settings":{"clusterName":"x"}}`, []string{"settings.clusterName"}},
		{"null at the key", `{"settings":{"clusterName":null}}`, []string{"settings.clusterName"}},
		{"below the key", `{"serviceAccount":{"annotations":{"eks.amazonaws.com/role-arn":"x"}}}`, []string{"serviceAccount.annotations"}},
		{"a whole protected map", `{"defaultNodeClass":{"role":"x"}}`, []string{"defaultNodeClass"}},
		{"a parent that is not an object replaces it", `{"settings":"x"}`, []string{"settings"}},
		{"a null parent replaces it", `{"serviceAccount":null}`, []string{"serviceAccount"}},
		{"an object parent without the key is fine", `{"serviceAccount":{"name":"x"}}`, nil},
		{"every refused path, sorted, once", `{"settings":{"interruptionQueue":"q","clusterName":"c"},"defaultNodeClass":1}`,
			[]string{"defaultNodeClass", "settings.clusterName", "settings.interruptionQueue"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProtectedPaths(parse(t, tt.tenant), protected); !reflect.DeepEqual(got, tt.want) && (len(got) != 0 || len(tt.want) != 0) {
				t.Errorf("ProtectedPaths = %v, want %v", got, tt.want)
			}
		})
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
