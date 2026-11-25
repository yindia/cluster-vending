package aws

import (
	"encoding/json"
	"slices"

	"sigs.k8s.io/yaml"
)

// addonPlacement is how an add-on's configuration places its controller
// pods: the objects that take "nodeSelector" and "tolerations", each by
// its keys (none: the top level only; an empty path is the top level),
// and the tolerations the add-on has by default. EKS replaces a list as a
// whole, so the defaults are written again after the pool's.
type addonPlacement struct {
	paths              [][]string
	defaultTolerations []map[string]any
}

// topLevel places an add-on's pods with keys at the top of its
// configuration.
var topLevel = [][]string{nil}

// controller places an add-on's controller pods (its node DaemonSet
// tolerates every taint already).
var controller = [][]string{{"controller"}}

// addonPlacements are the add-ons whose controller pods are placed by the
// platform: on the default node pool, the system group (its labels become
// their nodeSelector, and they tolerate each of its taints). With the
// tainted system group the platform's pods run there and tenant workloads
// do not.
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
// The schemas of these were read with describe-addon-configuration
// (us-west-1, Kubernetes 1.35, 2026-11-25):
//
//	metrics-server                v0.9.1-eksbuild.2    nodeSelector, tolerations
//	kube-state-metrics            v2.20.0-eksbuild.11  nodeSelector, tolerations
//	snapshot-controller           v8.7.0-eksbuild.5    nodeSelector, tolerations
//	cert-manager                  v1.21.3-eksbuild.1   nodeSelector, tolerations, and the same
//	                                                   under cainjector and webhook
//	aws-efs-csi-driver            v3.6.0-eksbuild.1    controller.nodeSelector, controller.tolerations
//	aws-fsx-csi-driver            v1.11.0-eksbuild.1   controller.nodeSelector, controller.tolerations
//	aws-mountpoint-s3-csi-driver  v2.9.2-eksbuild.1    controller.nodeSelector, controller.tolerations
//
// Their charts have no default tolerations. Without a placement these
// add-ons wait for tenant nodes: on a cluster without tenant node groups
// their pods never run (only the tainted system group and Karpenter's
// tainted platform pool exist).
//
// Not placed: DaemonSets that run on every node and tolerate every taint
// already: vpc-cni, kube-proxy, eks-pod-identity-agent and the node pods
// of the EBS, EFS, FSx and Mountpoint CSI drivers (node.tolerateAllTaints,
// operator Exists in their charts).
var addonPlacements = map[string]addonPlacement{
	"coredns": {paths: topLevel, defaultTolerations: []map[string]any{
		{"key": "CriticalAddonsOnly", "operator": "Exists"},
		{"key": "node-role.kubernetes.io/control-plane", "effect": "NoSchedule"},
	}},
	AddonEBSCSI: {paths: controller, defaultTolerations: []map[string]any{
		{"key": "CriticalAddonsOnly", "operator": "Exists"},
		{"effect": "NoExecute", "operator": "Exists", "tolerationSeconds": 300},
	}},
	"metrics-server":               {paths: topLevel},
	"kube-state-metrics":           {paths: topLevel},
	"snapshot-controller":          {paths: topLevel},
	"cert-manager":                 {paths: [][]string{nil, {"cainjector"}, {"webhook"}}},
	"aws-efs-csi-driver":           {paths: controller},
	"aws-fsx-csi-driver":           {paths: controller},
	"aws-mountpoint-s3-csi-driver": {paths: controller},
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
