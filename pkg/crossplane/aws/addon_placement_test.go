package aws_test

import (
	"encoding/json"
	"reflect"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// addonConfigurations renders p and returns each add-on's
// configurationValues by add-on name; an add-on without one is absent.
func addonConfigurations(t *testing.T, p crossplaneaws.ClusterPlan) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, o := range crossplaneaws.ClusterResources(p) {
		addon, ok := o.(*awseks.Addon)
		if !ok || addon.Spec.ForProvider.ConfigurationValues == nil {
			continue
		}
		var values map[string]any
		if err := json.Unmarshal([]byte(*addon.Spec.ForProvider.ConfigurationValues), &values); err != nil {
			t.Fatalf("%s: configurationValues %q: %v", o.GetName(), *addon.Spec.ForProvider.ConfigurationValues, err)
		}
		out[*addon.Spec.ForProvider.AddonName] = values
	}
	return out
}

// allAddons is every add-on the API accepts (the Enum of AWSAddon.name).
var allAddons = []string{"vpc-cni", "coredns", "kube-proxy", "eks-pod-identity-agent", "aws-ebs-csi-driver",
	"aws-efs-csi-driver", "aws-fsx-csi-driver", "aws-mountpoint-s3-csi-driver", "snapshot-controller", "cert-manager",
	"metrics-server", "kube-state-metrics"}

func planWithAddons(configurations map[string]string) crossplaneaws.ClusterPlan {
	p := clusterPlan()
	p.Addons = nil
	for _, name := range allAddons {
		p.Addons = append(p.Addons, crossplaneaws.AddonPlan{Name: name, ConfigurationValues: configurations[name],
			S3BucketNames: []string{"data"}})
	}
	return p
}

var (
	poolTolerations = []any{
		map[string]any{"key": "dedicated", "operator": "Equal", "value": "system", "effect": "NoSchedule"},
		map[string]any{"key": "critical", "operator": "Equal", "effect": "NoExecute"},
	}
	corednsDefaults = []any{
		map[string]any{"key": "CriticalAddonsOnly", "operator": "Exists"},
		map[string]any{"key": "node-role.kubernetes.io/control-plane", "effect": "NoSchedule"},
	}
	ebsDefaults = []any{
		map[string]any{"key": "CriticalAddonsOnly", "operator": "Exists"},
		map[string]any{"effect": "NoExecute", "operator": "Exists", "tolerationSeconds": float64(300)},
	}
)

func TestAddonsFollowTheDefaultNodePool(t *testing.T) {
	labels := map[string]string{"role": "system", "team": "platform"}
	taints := []crossplaneaws.TaintPlan{
		{Key: "dedicated", Value: "system", Effect: "NO_SCHEDULE"}, {Key: "critical", Effect: "NO_EXECUTE"}}
	selector := map[string]any{"role": "system", "team": "platform"}
	tests := []struct {
		name           string
		labels         map[string]string
		taints         []crossplaneaws.TaintPlan
		configurations map[string]string
		imported       bool
		want           map[string]map[string]any
	}{
		{name: "no labels and no taints: nothing set", want: map[string]map[string]any{}},
		{name: "no labels and no taints: the tenant's configuration as it is",
			configurations: map[string]string{"coredns": `{"replicaCount":3}`},
			want:           map[string]map[string]any{"coredns": {"replicaCount": float64(3)}}},
		{name: "labels and taints", labels: labels, taints: taints,
			want: map[string]map[string]any{
				"coredns": {"nodeSelector": selector, "tolerations": append(append([]any{}, poolTolerations...), corednsDefaults...)},
				"aws-ebs-csi-driver": {"controller": map[string]any{"nodeSelector": selector,
					"tolerations": append(append([]any{}, poolTolerations...), ebsDefaults...)}},
			}},
		{name: "labels only: no tolerations", labels: labels,
			want: map[string]map[string]any{
				"coredns":            {"nodeSelector": selector},
				"aws-ebs-csi-driver": {"controller": map[string]any{"nodeSelector": selector}},
			}},
		{name: "taints only: no nodeSelector", taints: taints[:1],
			want: map[string]map[string]any{
				"coredns": {"tolerations": append([]any{poolTolerations[0]}, corednsDefaults...)},
				"aws-ebs-csi-driver": {"controller": map[string]any{
					"tolerations": append([]any{poolTolerations[0]}, ebsDefaults...)}},
			}},
		{name: "the tenant's settings stay, and its own placement wins", labels: labels, taints: taints,
			configurations: map[string]string{
				"coredns":            "replicaCount: 3\nnodeSelector:\n  mine: \"yes\"\n", // YAML is accepted
				"aws-ebs-csi-driver": `{"controller":{"extraVolumeTags":{"a":"b"},"tolerations":[]},"node":{"tolerateAllTaints":true}}`,
				"vpc-cni":            `{"env":{"ENABLE_PREFIX_DELEGATION":"true"}}`,
			},
			want: map[string]map[string]any{
				"coredns": {"replicaCount": float64(3), "nodeSelector": map[string]any{"mine": "yes"},
					"tolerations": append(append([]any{}, poolTolerations...), corednsDefaults...)},
				"aws-ebs-csi-driver": {"controller": map[string]any{"extraVolumeTags": map[string]any{"a": "b"},
					"tolerations": []any{}, "nodeSelector": selector}, "node": map[string]any{"tolerateAllTaints": true}},
				"vpc-cni": {"env": map[string]any{"ENABLE_PREFIX_DELEGATION": "true"}},
			}},
		{name: "an imported cluster's add-ons stay where they are", labels: labels, taints: taints, imported: true,
			want: map[string]map[string]any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := planWithAddons(tt.configurations)
			p.NodePool.Labels, p.NodePool.Taints = tt.labels, tt.taints
			if tt.imported {
				p.ExternalNames = map[string]string{"prod-1a2b3c4d-default": "ng-1"}
			}

			got := addonConfigurations(t, p)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %v\nwant %v", got, tt.want)
			}
		})
	}
}

// A configuration the platform cannot read goes to EKS as it is.
func TestAddonPlacementLeavesUnreadableConfigurationAlone(t *testing.T) {
	for _, raw := range []string{"[1,2]", `{"controller":"x"}`, "{"} {
		p := clusterPlan()
		p.NodePool.Labels = map[string]string{"role": "system"}
		p.Addons = []crossplaneaws.AddonPlan{{Name: "aws-ebs-csi-driver", ConfigurationValues: raw}}
		for _, o := range crossplaneaws.ClusterResources(p) {
			if addon, ok := o.(*awseks.Addon); ok && *addon.Spec.ForProvider.ConfigurationValues != raw {
				t.Errorf("%q became %q", raw, *addon.Spec.ForProvider.ConfigurationValues)
			}
		}
	}
}

func TestTolerationsOf(t *testing.T) {
	got := crossplaneaws.TolerationsOf([]crossplaneaws.TaintPlan{
		{Key: "a", Value: "b", Effect: "PREFER_NO_SCHEDULE"}, {Key: "c", Effect: "NoSchedule"}})

	want := []map[string]any{
		{"key": "a", "operator": "Equal", "value": "b", "effect": "PreferNoSchedule"},
		{"key": "c", "operator": "Equal", "effect": "NoSchedule"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := crossplaneaws.TolerationsOf(nil); got == nil || len(got) != 0 {
		t.Errorf("no taints = %v, want an empty list", got)
	}
}

func TestDedicatedPoolWarning(t *testing.T) {
	tests := []struct {
		name   string
		taints []crossplaneaws.TaintPlan
		warns  bool
	}{
		{name: "no taints"},
		{name: "a soft taint only", taints: []crossplaneaws.TaintPlan{{Key: "spot", Effect: "PREFER_NO_SCHEDULE"}}},
		{name: "NoSchedule", taints: []crossplaneaws.TaintPlan{{Key: "dedicated", Value: "system", Effect: "NO_SCHEDULE"}}, warns: true},
		{name: "NoExecute after a soft one", taints: []crossplaneaws.TaintPlan{
			{Key: "spot", Effect: "PREFER_NO_SCHEDULE"}, {Key: "critical", Effect: "NO_EXECUTE"}}, warns: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := crossplaneaws.DedicatedPoolWarning(crossplaneaws.NodePoolPlan{Taints: tt.taints})
			if (got != "") != tt.warns {
				t.Errorf("warning = %q, want one: %v", got, tt.warns)
			}
		})
	}
}
