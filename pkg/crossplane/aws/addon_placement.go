package aws

import (
	"encoding/json"
	"slices"

	"sigs.k8s.io/yaml"
)

// addonPlacement is how an add-on's configuration places its controller
// pods (placement in addons.yaml): the objects that take "nodeSelector" and
// "tolerations", each by its keys (nil: the top level), and the
// tolerations the add-on has by default. EKS replaces a list as a whole,
// so the defaults are written again after the pool's.
//
// The placed add-ons' controllers run on the default node pool, the system
// group: its labels become their nodeSelector, and they tolerate each of
// its taints. With the tainted system group the platform's pods run there
// and tenant workloads do not.
type addonPlacement struct {
	paths              [][]string
	defaultTolerations []map[string]any
}

// taintEffects maps the EKS spelling of a taint effect to Kubernetes'.
var taintEffects = map[string]string{
	"NO_SCHEDULE": "NoSchedule", "PREFER_NO_SCHEDULE": "PreferNoSchedule", "NO_EXECUTE": "NoExecute",
}

// KubernetesTaintEffect is Kubernetes' spelling of an EKS taint effect
// (NO_SCHEDULE: NoSchedule); one it does not know as it is.
func KubernetesTaintEffect(effect string) string {
	if k8s, ok := taintEffects[effect]; ok {
		return k8s
	}
	return effect
}

// TolerationsOf are tolerations of taints, one each (operator Equal), as a
// pod spec takes them.
func TolerationsOf(taints []TaintPlan) []map[string]any {
	out := make([]map[string]any, 0, len(taints))
	for _, t := range taints {
		toleration := map[string]any{"key": t.Key, "operator": "Equal", "effect": KubernetesTaintEffect(t.Effect)}
		if t.Value != "" {
			toleration["value"] = t.Value
		}
		out = append(out, toleration)
	}
	return out
}

// placedConfiguration is the add-on's configurationValues with the default
// node pool's placement added (placement in addons.yaml). It is the tenant's
// configuration as it is when the pool has neither labels nor taints, for
// an add-on that is not placed, for an imported cluster (its add-ons stay
// where they run) and when the configuration is not a JSON or YAML
// object (EKS reports that). A nodeSelector or tolerations the tenant set
// wins: only missing keys are added.
func (b clusterBuilder) placedConfiguration(a AddonPlan) string {
	pool := b.p.NodePool
	placement, ok := addonCatalog.placements[a.Name]
	if !ok || len(b.p.ExternalNames) > 0 {
		return a.ConfigurationValues
	}
	labels, taints := pool.Labels, pool.Taints
	if len(labels) == 0 && len(taints) == 0 {
		return a.ConfigurationValues
	}
	values := map[string]any{}
	if a.ConfigurationValues != "" {
		if err := yaml.Unmarshal([]byte(a.ConfigurationValues), &values); err != nil || values == nil {
			return a.ConfigurationValues
		}
	}
	for _, path := range placement.paths {
		target, ok := objectAt(values, path)
		if !ok {
			return a.ConfigurationValues
		}
		if _, set := target["nodeSelector"]; !set && len(labels) > 0 {
			target["nodeSelector"] = labels
		}
		if _, set := target["tolerations"]; !set && len(taints) > 0 {
			target["tolerations"] = slices.Concat(TolerationsOf(taints), placement.defaultTolerations)
		}
	}
	out, err := json.Marshal(values) // keys sorted: the same plan renders the same string
	if err != nil {
		return a.ConfigurationValues
	}
	return string(out)
}

// objectAt is the object of values at path, made when missing; false when
// a key on the way holds something other than an object.
func objectAt(values map[string]any, path []string) (map[string]any, bool) {
	target := values
	for _, key := range path {
		child, isMap := target[key].(map[string]any)
		if _, set := target[key]; set && !isMap {
			return nil, false
		}
		if child == nil {
			child = map[string]any{}
			target[key] = child
		}
		target = child
	}
	return target, true
}
