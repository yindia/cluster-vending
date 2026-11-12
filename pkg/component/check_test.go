package component

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/fault"
)

// parseValues is a values literal written as JSON; it panics on a typo.
func parseValues(raw string) Values {
	var v Values
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		panic(raw + ": " + err.Error())
	}
	return v
}

func asJSONString(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// karpenterDefinition is the design's Karpenter definition (section 6.1).
func karpenterDefinition() Definition {
	return Definition{
		Name: "karpenter", Valid: true,
		Defaults: parseValues(`{"replicas":2,"controller":{"resources":{"requests":{"cpu":"250m","memory":"512Mi"}}},
			"defaultNodePool":{"enabled":true,"limits":{"cpu":"64"},
				"requirements":[{"key":"karpenter.sh/capacity-type","operator":"In","values":["on-demand"]}]}}`),
		ProtectedKeys: []string{"settings.clusterName", "settings.clusterEndpoint", "settings.interruptionQueue",
			"serviceAccount.name", "serviceAccount.annotations", "defaultNodeClass"},
		ValuesSchema: parseValues(`{"type":"object","additionalProperties":false,"properties":{
			"replicas":{"type":"integer","minimum":1,"maximum":3},
			"logLevel":{"type":"string","enum":["error","info","debug"]},
			"controller":{"type":"object"},
			"settings":{"type":"object","properties":{
				"featureGates":{"type":"object","additionalProperties":{"type":"boolean"}},
				"batchMaxDuration":{"type":"string"}}},
			"defaultNodePool":{"type":"object","properties":{
				"enabled":{"type":"boolean"},"limits":{"type":"object"},"requirements":{"type":"array"},"disruption":{"type":"object"}}}}}`),
	}
}

// karpenterTenantValues are the tenant values of the design (section 6.3).
func karpenterTenantValues(t *testing.T) Values {
	t.Helper()
	return parseValues(`{"replicas":2,"logLevel":"info","settings":{"featureGates":{"spotToSpotConsolidation":true}},
		"defaultNodePool":{"limits":{"cpu":"128"},"requirements":[
			{"key":"karpenter.sh/capacity-type","operator":"In","values":["spot","on-demand"]},
			{"key":"node.kubernetes.io/instance-type","operator":"In","values":["m6i.large","m6i.xlarge"]}]}}`)
}

// running is a manual cluster on 1.37 with the default add-ons.
func running() *Cluster {
	return &Cluster{Name: "prod-eks", State: "RUNNING", Running: true, Mode: Manual, KubernetesMinor: "1.37",
		Addons: []string{"vpc-cni", "coredns", "kube-proxy", "eks-pod-identity-agent", "aws-ebs-csi-driver"}}
}

// offering is what release 2026-11-a offers on 1.37.
func offering() *Offering {
	return &Offering{Release: "2026-11-a", Supported: true,
		Versions: map[string]string{"karpenter": "1.8.1", "kyverno": "3.5.0", "kyverno-policies": "3.5.0", "cert-manager": "1.19.1"},
		Pinnable: map[string][]string{"karpenter": {"1.8.1", "1.7.4"}}}
}

func defs() map[string]Definition {
	return map[string]Definition{
		"karpenter":        karpenterDefinition(),
		"kyverno":          {Name: "kyverno", Valid: true, ValuesSchema: parseValues(`{"type":"object"}`)},
		"kyverno-policies": {Name: "kyverno-policies", Valid: true, Requires: []string{"kyverno"}},
		"cert-manager":     {Name: "cert-manager", Valid: true, ConflictsWithAddons: []string{"cert-manager"}},
	}
}

// request is an enable of name on a running manual cluster, release
// 2026-11-a offering it, with change applied.
func request(name string, change func(*Request)) Request {
	d := defs()[name]
	r := Request{Component: name, Definition: &d, Cluster: running(), Offering: offering()}
	if change != nil {
		change(&r)
	}
	return r
}

func TestCheckEnable(t *testing.T) {
	for _, tt := range []struct {
		name   string
		r      Request
		checks []int  // the problems' check numbers, in order
		want   string // a text the error contains
		kind   fault.Kind
	}{
		{"karpenter with the design's values", request("karpenter", func(r *Request) { r.Values = karpenterTenantValues(t) }), nil, "", 0},
		{"1 not in the catalog", request("karpenter", func(r *Request) { r.Definition = nil }), []int{1},
			"component karpenter is not in the catalog", fault.NotFound},
		{"1 an invalid definition counts as missing", request("karpenter", func(r *Request) { r.Definition.Valid = false }), []int{1},
			"not in the catalog", fault.NotFound},
		{"2 no such cluster", request("karpenter", func(r *Request) { r.Cluster = nil }), []int{2}, "no such cluster", fault.NotFound},
		{"2 deleting", request("karpenter", func(r *Request) { r.Cluster.Deleting = true }), []int{2}, "being deleted", fault.FailedPrecondition},
		{"2 imported", request("karpenter", func(r *Request) { r.Cluster.Imported = true }), []int{2},
			"take the cluster over first", fault.FailedPrecondition},
		{"3 not running", request("karpenter", func(r *Request) { r.Cluster.Running, r.Cluster.State = false, "PROVISIONING" }), []int{3},
			"the cluster is PROVISIONING; enable components once it is RUNNING", fault.FailedPrecondition},
		{"4 already enabled", request("karpenter", func(r *Request) { r.Enabled = []string{"karpenter"} }), []int{4},
			"already enabled", fault.AlreadyExists},
		{"4 an update of the enabled component is fine", request("karpenter", func(r *Request) { r.Enabled, r.Update = []string{"karpenter"}, true }),
			nil, "", 0},
		{"5 no release applies", request("karpenter", func(r *Request) { r.Offering = nil }), []int{5},
			"no release applies to cluster prod-eks", fault.FailedPrecondition},
		{"5 not offered on the minor", request("karpenter", func(r *Request) { delete(r.Offering.Versions, "karpenter") }), []int{5},
			"karpenter is not offered on Kubernetes 1.37 in release 2026-11-a", fault.FailedPrecondition},
		{"6 automatic clusters may not pin", request("karpenter", func(r *Request) { r.Cluster.Mode, r.Version = Automatic, "1.8.1" }), []int{6},
			"cluster prod-eks is automatic", fault.Invalid},
		{"6 a manual pin a release lists", request("karpenter", func(r *Request) { r.Version = "1.7.4" }), nil, "", 0},
		{"6 a manual pin no release lists", request("karpenter", func(r *Request) { r.Version = "1.6.0" }), []int{6},
			"version 1.6.0 of karpenter is not listed for Kubernetes 1.37 by any release; listed: 1.7.4, 1.8.1", fault.Invalid},
		{"7 a dependency not enabled", request("kyverno-policies", nil), []int{7}, "enable kyverno first", fault.FailedPrecondition},
		{"7 the dependency enabled", request("kyverno-policies", func(r *Request) { r.Enabled = []string{"kyverno"} }), nil, "", 0},
		{"8 the add-on it conflicts with", request("cert-manager", func(r *Request) { r.Cluster.Addons = append(r.Cluster.Addons, "cert-manager") }),
			[]int{8}, "remove the cert-manager add-on first", fault.FailedPrecondition},
		{"8 without the add-on", request("cert-manager", nil), nil, "", 0},
		{"9 too large", request("kyverno", func(r *Request) { r.Values = Values{"a": strings.Repeat("x", MaxValuesSize)} }), []int{9},
			"at most 65536", fault.Invalid},
		{"10 protected keys, every one named", request("karpenter", func(r *Request) {
			r.Values = parseValues(`{"settings":{"clusterName":"x","interruptionQueue":"q"},"defaultNodeClass":{"role":"r"}}`)
		}), []int{10}, "only the platform sets defaultNodeClass, settings.clusterName, settings.interruptionQueue", fault.Invalid},
		{"11 schema", request("karpenter", func(r *Request) { r.Values = parseValues(`{"replicas":9,"logLevel":"trace"}`) }), []int{11, 11},
			"replicas: must be at most 3", fault.Invalid},
		{"11 no schema accepts no values", request("cert-manager", func(r *Request) { r.Values = Values{"a": 1} }), []int{11},
			"component cert-manager accepts no values", fault.Invalid},
		{"12 outside the support window", request("karpenter", func(r *Request) { r.Offering.Supported = false }), []int{12},
			"outside the support window of release 2026-11-a", fault.FailedPrecondition},
		{"every problem at once, invalid wins", request("kyverno-policies", func(r *Request) {
			r.Cluster.Running, r.Cluster.State, r.Version, r.Cluster.Mode = false, "UPDATING", "1.0.0", Automatic
			r.Values = Values{"a": 1}
		}), []int{3, 6, 7, 11}, "enable kyverno first", fault.Invalid},
		{"several preconditions", request("kyverno-policies", func(r *Request) { r.Offering.Supported = false }), []int{12, 7},
			"outside the support window", fault.FailedPrecondition},
	} {
		t.Run(tt.name, func(t *testing.T) {
			problems := CheckEnable(tt.r)
			var checks []int
			for _, p := range problems {
				checks = append(checks, p.Check)
			}
			if !reflect.DeepEqual(checks, tt.checks) {
				t.Fatalf("checks %v, want %v: %+v", checks, tt.checks, problems)
			}
			err := Err(problems)
			if tt.checks == nil {
				if err != nil {
					t.Fatalf("Err = %v, want none", err)
				}
				return
			}
			f, ok := fault.As(err)
			if !ok || f.Kind != tt.kind || !strings.Contains(f.Message, tt.want) {
				t.Errorf("Err = %v (kind %v), want kind %v containing %q", err, f, tt.kind, tt.want)
			}
		})
	}
}

func TestCheckDisable(t *testing.T) {
	d := defs()
	for _, tt := range []struct {
		name    string
		disable string
		enabled []Definition
		want    string
	}{
		{"nothing needs it", "kyverno-policies", []Definition{d["kyverno"], d["kyverno-policies"]}, ""},
		{"a dependant is enabled", "kyverno", []Definition{d["kyverno"], d["kyverno-policies"]},
			"kyverno is required by kyverno-policies; disable kyverno-policies first"},
		{"alone", "kyverno", []Definition{d["kyverno"]}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckDisable(tt.disable, tt.enabled)
			if tt.want == "" {
				if err != nil {
					t.Errorf("CheckDisable = %v, want none", err)
				}
				return
			}
			if f, ok := fault.As(err); !ok || f.Kind != fault.FailedPrecondition || f.Message != tt.want {
				t.Errorf("CheckDisable = %v, want FailedPrecondition %q", err, tt.want)
			}
		})
	}
}

// Adding the add-on a component conflicts with is refused too (the
// reverse of check 8, for UpdateCluster).
func TestCheckAddons(t *testing.T) {
	d := defs()
	if err := CheckAddons([]string{"vpc-cni", "cert-manager"}, []Definition{d["cert-manager"]}); err == nil ||
		!strings.Contains(err.Error(), "the cert-manager add-on conflicts with component cert-manager") {
		t.Errorf("CheckAddons = %v, want the conflict", err)
	}
	if err := CheckAddons([]string{"vpc-cni", "cert-manager"}, []Definition{d["karpenter"]}); err != nil {
		t.Errorf("CheckAddons without a conflicting component = %v", err)
	}
}
