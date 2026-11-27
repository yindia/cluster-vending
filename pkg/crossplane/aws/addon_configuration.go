package aws

import (
	"encoding/json"
	"strconv"

	"sigs.k8s.io/yaml"
)

// addonConfiguration is the add-on's configurationValues as rendered: the
// tenant's, with the default node pool's placement added (only keys the
// tenant left unset) and the add-on's enforced keys written over it
// (configuration in addons.yaml). The enforced keys win because they carry
// a platform guarantee (vpc-cni enforces NetworkPolicy on every cluster);
// the tenant's other keys, nested ones included, stay.
//
// It is the tenant's configuration as it is when there is nothing to add,
// and when the configuration is not a JSON or YAML object or a placement
// path holds something other than an object: EKS refuses it and the add-on
// reports why.
func (b clusterBuilder) addonConfiguration(a AddonPlan) string {
	enforced := addonCatalog.configurations[a.Name]
	placement, placed := b.placeable(a)
	if len(enforced) == 0 && !placed {
		return a.ConfigurationValues
	}
	values := map[string]any{}
	if a.ConfigurationValues != "" {
		if err := yaml.Unmarshal([]byte(a.ConfigurationValues), &values); err != nil || values == nil {
			return a.ConfigurationValues
		}
	}
	if placed && !b.place(values, placement) {
		return a.ConfigurationValues
	}
	values = overlay(values, enforced)
	out, err := json.Marshal(values) // keys sorted: the same plan renders the same string
	if err != nil {
		return a.ConfigurationValues
	}
	return string(out)
}

// overlay is base with over's keys written over it: objects on both sides
// merge key by key, anything else from over replaces base's. Neither input
// is changed.
func overlay(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		overMap, isMap := v.(map[string]any)
		if !isMap {
			out[k] = v
			continue
		}
		baseMap, _ := out[k].(map[string]any)
		out[k] = overlay(baseMap, overMap)
	}
	return out
}

// VPCCNIAddon is the EKS add-on whose configuration decides whether
// NetworkPolicy is enforced (by its node agent).
const VPCCNIAddon = "vpc-cni"

// NetworkPolicyEnforced reports whether a vpc-cni configurationValues
// turns NetworkPolicy enforcement on: enableNetworkPolicy true (in any
// spelling EKS's schema accepts) and the node agent not turned off.
func NetworkPolicyEnforced(configurationValues string) bool {
	if configurationValues == "" {
		return false
	}
	var values struct {
		EnableNetworkPolicy string `json:"enableNetworkPolicy"`
		NodeAgent           *struct {
			Enabled *bool `json:"enabled"`
		} `json:"nodeAgent"`
	}
	if err := yaml.Unmarshal([]byte(configurationValues), &values); err != nil {
		return false
	}
	on, err := strconv.ParseBool(values.EnableNetworkPolicy)
	if err != nil || !on {
		return false
	}
	return values.NodeAgent == nil || values.NodeAgent.Enabled == nil || *values.NodeAgent.Enabled
}
