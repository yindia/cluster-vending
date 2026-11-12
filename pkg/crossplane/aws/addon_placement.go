package aws

import (
	"encoding/json"
	"slices"

	"sigs.k8s.io/yaml"
)

// addonPlacement is how an add-on's configuration places its controller
// pods: the keys of the object that takes "nodeSelector" and
// "tolerations" (none: the top level), and the tolerations the add-on
// has by default. EKS replaces a list as a whole, so the defaults are
// written again after the pool's.
type addonPlacement struct {
	path               []string
	defaultTolerations []map[string]any
}

// addonPlacements are the add-ons whose controller pods follow the
// default node pool: its labels become their nodeSelector, and they
// tolerate each of its taints. With a tainted default pool the platform's
// pods then run there and tenant workloads do not.
//
// Only add-ons whose configuration schema (aws eks
// describe-addon-configuration) is confirmed are listed; EKS refuses a
// configuration with a key its schema lacks, so no key is guessed:
//
//	add-on              keys                                source
//	coredns             nodeSelector, tolerations           schema of v1.11.1-eksbuild.8, quoted in
//	                                                        terraform-aws-eks docs/faq.md and
//	                                                        https://aws-ia.github.io/terraform-aws-eks-blueprints-addons/main/amazon-eks-addons
//	aws-ebs-csi-driver  controller.nodeSelector,            the add-on's schema (it is the chart's values.schema.json):
//	                    controller.tolerations              https://gist.github.com/kcemenike-sb/1939bc036d201b21c2f1afc48934e4fd,
//	                                                        https://github.com/kubernetes-sigs/aws-ebs-csi-driver/blob/master/charts/aws-ebs-csi-driver/values.yaml
//
// Not placed:
//   - DaemonSets that run on every node and tolerate every taint already:
//     vpc-cni, kube-proxy, eks-pod-identity-agent and the node pods of the
//     EBS, EFS and FSx CSI drivers (node.tolerateAllTaints, operator
//     Exists in their charts).
//   - Add-ons whose schema is not confirmed from a public source: the
//     controllers of aws-efs-csi-driver and aws-fsx-csi-driver (their
//     charts have controller.nodeSelector and controller.tolerations, the
//     add-on schema is unchecked), aws-mountpoint-s3-csi-driver,
//     snapshot-controller, metrics-server, kube-state-metrics and
//     cert-manager. A tenant sets their placement in the add-on's
//     configurationValues.
var addonPlacements = map[string]addonPlacement{
	"coredns": {defaultTolerations: []map[string]any{
		{"key": "CriticalAddonsOnly", "operator": "Exists"},
		{"key": "node-role.kubernetes.io/control-plane", "effect": "NoSchedule"},
	}},
	AddonEBSCSI: {path: []string{"controller"}, defaultTolerations: []map[string]any{
		{"key": "CriticalAddonsOnly", "operator": "Exists"},
		{"effect": "NoExecute", "operator": "Exists", "tolerationSeconds": 300},
	}},
}

// taintEffects maps the EKS spelling of a taint effect to Kubernetes'.
var taintEffects = map[string]string{
	"NO_SCHEDULE": "NoSchedule", "PREFER_NO_SCHEDULE": "PreferNoSchedule", "NO_EXECUTE": "NoExecute",
}

// TolerationsOf are tolerations of taints, one each (operator Equal), as a
// pod spec takes them.
func TolerationsOf(taints []TaintPlan) []map[string]any {
	out := make([]map[string]any, 0, len(taints))
	for _, t := range taints {
		effect := t.Effect
		if k8s, ok := taintEffects[effect]; ok {
			effect = k8s
		}
		toleration := map[string]any{"key": t.Key, "operator": "Equal", "effect": effect}
		if t.Value != "" {
			toleration["value"] = t.Value
		}
		out = append(out, toleration)
	}
	return out
}

// placedConfiguration is the add-on's configurationValues with the default
// node pool's placement added (addonPlacements). It is the tenant's
// configuration as it is when the pool has neither labels nor taints, for
// an add-on that is not placed, for an imported cluster (its add-ons stay
// where they run) and when the configuration is not a JSON or YAML
// object (EKS reports that). A nodeSelector or tolerations the tenant set
// wins: only missing keys are added.
func (b clusterBuilder) placedConfiguration(a AddonPlan) string {
	pool := b.p.NodePool
	placement, ok := addonPlacements[a.Name]
	if !ok || len(pool.Labels) == 0 && len(pool.Taints) == 0 || len(b.p.ExternalNames) > 0 {
		return a.ConfigurationValues
	}
	values := map[string]any{}
	if a.ConfigurationValues != "" {
		if err := yaml.Unmarshal([]byte(a.ConfigurationValues), &values); err != nil || values == nil {
			return a.ConfigurationValues
		}
	}
	target := values
	for _, key := range placement.path {
		child, isMap := target[key].(map[string]any)
		if _, set := target[key]; set && !isMap {
			return a.ConfigurationValues
		}
		if child == nil {
			child = map[string]any{}
			target[key] = child
		}
		target = child
	}
	if _, set := target["nodeSelector"]; !set && len(pool.Labels) > 0 {
		target["nodeSelector"] = pool.Labels
	}
	if _, set := target["tolerations"]; !set && len(pool.Taints) > 0 {
		target["tolerations"] = slices.Concat(TolerationsOf(pool.Taints), placement.defaultTolerations)
	}
	out, err := json.Marshal(values) // keys sorted: the same plan renders the same string
	if err != nil {
		return a.ConfigurationValues
	}
	return string(out)
}

// DedicatedPoolWarning says, for a default node pool with a taint that
// keeps pods off it (NoSchedule, NoExecute), that tenant workloads need
// nodes of their own; empty otherwise. The platform's components and the
// placed add-ons tolerate the pool's taints; nothing else does. It is a
// warning, not a refusal: the nodes may come later (Karpenter).
func DedicatedPoolWarning(pool NodePoolPlan) string {
	for _, t := range pool.Taints {
		switch taintEffects[t.Effect] {
		case "NoSchedule", "NoExecute":
			return "the default node pool is tainted (" + t.Key + ", " + taintEffects[t.Effect] + "): only the platform's components " +
				"and pods that tolerate the taint run on it; workloads need other nodes (the karpenter component) or a toleration"
		}
	}
	return ""
}
