package aws

import (
	"slices"
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

// placeable is the add-on's placement when it applies to this cluster:
// the add-on is placed, the cluster is not imported (its add-ons stay where
// they run) and the default node pool has labels or taints.
func (b clusterBuilder) placeable(a AddonPlan) (addonPlacement, bool) {
	placement, ok := addonCatalog.placements[a.Name]
	if !ok || len(b.p.ExternalNames) > 0 {
		return addonPlacement{}, false
	}
	pool := b.p.NodePool
	return placement, len(pool.Labels) > 0 || len(pool.Taints) > 0
}

// place adds the default node pool's placement to values (placement in
// addons.yaml). A nodeSelector or tolerations the tenant set wins: only
// missing keys are added. False when a path holds something other than an
// object.
func (b clusterBuilder) place(values map[string]any, placement addonPlacement) bool {
	labels, taints := b.p.NodePool.Labels, b.p.NodePool.Taints
	for _, path := range placement.paths {
		target, ok := objectAt(values, path)
		if !ok {
			return false
		}
		if _, set := target["nodeSelector"]; !set && len(labels) > 0 {
			target["nodeSelector"] = labels
		}
		if _, set := target["tolerations"]; !set && len(taints) > 0 {
			target["tolerations"] = slices.Concat(TolerationsOf(taints), placement.defaultTolerations)
		}
	}
	return true
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
