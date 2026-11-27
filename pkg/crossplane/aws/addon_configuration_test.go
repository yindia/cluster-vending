package aws_test

import (
	"maps"
	"reflect"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// networkPolicyKeys are the keys addons.yaml enforces in vpc-cni's
// configurationValues: the VPC CNI enforces NetworkPolicy (its node agent
// runs) on every cluster.
func networkPolicyKeys() map[string]any {
	return map[string]any{"enableNetworkPolicy": "true", "nodeAgent": map[string]any{"enabled": true}}
}

// enforced is want with vpc-cni's enforced keys merged in (the platform's
// win), as every cluster renders it.
func enforced(want map[string]map[string]any) map[string]map[string]any {
	out := maps.Clone(want)
	vpc := maps.Clone(out["vpc-cni"])
	if vpc == nil {
		vpc = map[string]any{}
	}
	maps.Copy(vpc, networkPolicyKeys())
	out["vpc-cni"] = vpc
	return out
}

func TestEveryClusterEnforcesNetworkPolicy(t *testing.T) {
	tests := []struct {
		name     string
		tenant   string
		imported bool
		want     map[string]any
	}{
		{name: "no configuration of the tenant's", want: networkPolicyKeys()},
		{name: "an imported cluster too", imported: true, want: networkPolicyKeys()},
		{name: "the tenant's keys stay", tenant: `{"env":{"ENABLE_PREFIX_DELEGATION":"true"},"nodeAgent":{"enablePolicyEventLogs":"true"}}`,
			want: map[string]any{"enableNetworkPolicy": "true", "env": map[string]any{"ENABLE_PREFIX_DELEGATION": "true"},
				"nodeAgent": map[string]any{"enabled": true, "enablePolicyEventLogs": "true"}}},
		{name: "the platform's keys win over the tenant's", tenant: "enableNetworkPolicy: \"false\"\nnodeAgent:\n  enabled: false\n",
			want: networkPolicyKeys()},
		{name: "a tenant's object in place of the platform's is replaced", tenant: `{"nodeAgent":"off"}`,
			want: networkPolicyKeys()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := clusterPlan()
			p.Addons = []crossplaneaws.AddonPlan{{Name: "vpc-cni", ConfigurationValues: tt.tenant}}
			if tt.imported {
				p.ExternalNames = map[string]string{"prod-1a2b3c4d-default": "ng-1"}
			}

			got := addonConfigurations(t, p)["vpc-cni"]

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %v\nwant %v", got, tt.want)
			}
		})
	}
}

// A vpc-cni configuration the platform cannot read goes to EKS as it is:
// EKS refuses it and the add-on reports why.
func TestNetworkPolicyLeavesUnreadableConfigurationAlone(t *testing.T) {
	for _, raw := range []string{"[1,2]", "{"} {
		p := clusterPlan()
		p.Addons = []crossplaneaws.AddonPlan{{Name: "vpc-cni", ConfigurationValues: raw}}
		for _, o := range crossplaneaws.ClusterResources(p) {
			if addon, ok := o.(*awseks.Addon); ok && *addon.Spec.ForProvider.ConfigurationValues != raw {
				t.Errorf("%q became %q", raw, *addon.Spec.ForProvider.ConfigurationValues)
			}
		}
	}
}

func TestNetworkPolicyEnforced(t *testing.T) {
	tests := []struct {
		name   string
		values string
		want   bool
	}{
		{name: "the platform's render", values: `{"enableNetworkPolicy":"true","nodeAgent":{"enabled":true}}`, want: true},
		{name: "EKS's spelling of true", values: `{"enableNetworkPolicy":"TRUE"}`, want: true},
		{name: "the node agent off", values: `{"enableNetworkPolicy":"true","nodeAgent":{"enabled":false}}`},
		{name: "off", values: `{"enableNetworkPolicy":"false"}`},
		{name: "unset", values: `{"env":{}}`},
		{name: "none", values: ""},
		{name: "unreadable", values: "{"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := crossplaneaws.NetworkPolicyEnforced(tt.values); got != tt.want {
				t.Errorf("NetworkPolicyEnforced(%q) = %t, want %t", tt.values, got, tt.want)
			}
		})
	}
}
