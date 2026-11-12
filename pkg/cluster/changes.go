package cluster

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// apiFields maps a managed resource's forProvider field (by prefix) to the
// API field it is rendered from, so a plan names what the client set.
var apiFields = map[string]map[string]string{
	"Cluster": {
		"version":                         "spec.control_plane.kubernetes_version",
		"vpcConfig.publicAccessCidrs":     "spec.control_plane.endpoint_access.public_access_cidrs",
		"vpcConfig.endpointPrivateAccess": "spec.control_plane.endpoint_access.endpoint_private_access",
		"vpcConfig.endpointPublicAccess":  "spec.control_plane.endpoint_access.endpoint_public_access",
		"vpcConfig.securityGroupIds":      "spec.aws.additional_security_group_ids",
		"encryptionConfig":                "spec.control_plane.secrets_encryption",
		"enabledClusterLogTypes":          "spec.aws.enabled_cluster_log_types",
		"upgradePolicy":                   "spec.aws.upgrade_policy.support_type",
		"accessConfig.authenticationMode": "spec.aws.access_config.authentication_mode",
		"tags":                            "labels",
	},
	"NodeGroup": {
		"version":               "spec.control_plane.kubernetes_version",
		"scalingConfig.minSize": "spec.default_node_pool.scaling_config.min_size",
		"scalingConfig.maxSize": "spec.default_node_pool.scaling_config.max_size",
		"labels":                "spec.default_node_pool.labels",
		"taint":                 "spec.default_node_pool.taints",
		"instanceTypes":         "spec.default_node_pool.instance_types",
		"capacityType":          "spec.default_node_pool.capacity_type",
		"diskSize":              "spec.default_node_pool.disk_size_gb",
		"amiType":               "spec.aws.default_node_pool.ami_type",
		"launchTemplate":        "spec.aws.default_node_pool.launch_template",
		"tags":                  "labels",
	},
	"Addon": {"addonVersion": "spec.aws.addons", "configurationValues": "spec.aws.addons", "tags": "labels"},
	"Group": {"retentionInDays": "spec.aws.control_plane_log_group.retention_days", "tags": "labels"},
}

// APIField is the API field of a kind's forProvider path, else the path
// itself ("forProvider.<path>").
func APIField(kind, path string) string {
	for p := path; ; {
		if f, ok := apiFields[kind][p]; ok {
			return f
		}
		i := strings.LastIndex(p, ".")
		if i < 0 {
			break
		}
		p = p[:i]
	}
	if strings.HasPrefix(path, "tags.") || path == "tags" {
		return "labels"
	}
	return "forProvider." + path
}

// ChangeKey identifies a planned change across two renders of a cluster.
func ChangeKey(c *cloudv1.PlannedChange) string {
	return strings.Join([]string{c.GetKind(), c.GetName(), c.GetAction().String(), c.GetSpecPath(), c.GetDetail()}, "|")
}

// CreateDetail says why an object a plan creates is created.
func CreateDetail(body *unstructured.Unstructured, replacing bool) string {
	if replacing && body.GetKind() == "NodeGroup" {
		return "created to replace the current node group (a setting EKS cannot change in place changed)"
	}
	return "does not exist yet"
}

// FieldChanges compares a live object with its dry-run apply, field by
// field of spec.forProvider and spec.initProvider, and with the cloud's
// state (status.atProvider): a field the update leaves alone but the cloud
// has differently was changed outside the platform, and is set back.
func FieldChanges(live, dry *unstructured.Unstructured) []*cloudv1.PlannedChange {
	var out []*cloudv1.PlannedChange
	change := func(path string, action cloudv1.PlannedAction, detail, diff string) {
		out = append(out, &cloudv1.PlannedChange{ApiVersion: dry.GetAPIVersion(), Kind: dry.GetKind(), Name: dry.GetName(),
			ExternalName: live.GetAnnotations()[crossplaneaws.ExternalNameAnnotation], SpecPath: APIField(dry.GetKind(), path),
			Action: action, Detail: detail, UnifiedDiff: diff})
	}
	header := fmt.Sprintf("--- %s/%s (live)\n+++ %s/%s (planned)\n", dry.GetKind(), dry.GetName(), dry.GetKind(), dry.GetName())
	was := fields(live, "spec", "forProvider")
	will := fields(dry, "spec", "forProvider")
	cloud := fields(live, "status", "atProvider")
	for _, path := range sortedKeys(was, will) {
		before, after := crossplaneaws.FieldString(was[path]), crossplaneaws.FieldString(will[path])
		if before != after {
			change(path, ActionFor(dry.GetKind(), path, was[path] == nil), fmt.Sprintf("forProvider.%s: %s -> %s", path, before, after),
				fmt.Sprintf("%s-forProvider.%s: %s\n+forProvider.%s: %s\n", header, path, before, path, after))
			continue
		}
		if actual, ok := cloud[path]; ok && isScalar(actual) && isScalar(will[path]) && crossplaneaws.FieldString(actual) != after {
			change(path, ActionFor(dry.GetKind(), path, false),
				fmt.Sprintf("forProvider.%s: the cloud has %s (changed outside the platform), set back to %s", path,
					crossplaneaws.FieldString(actual), after), "")
		}
	}
	wasInit, willInit := fields(live, "spec", "initProvider"), fields(dry, "spec", "initProvider")
	for _, path := range sortedKeys(wasInit, willInit) {
		before, after := crossplaneaws.FieldString(wasInit[path]), crossplaneaws.FieldString(willInit[path])
		if before != after {
			change(path, cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE,
				fmt.Sprintf("initProvider.%s: %s -> %s (used at creation only: no effect on the existing resource)", path, before, after), "")
		}
	}
	return out
}

func fields(u *unstructured.Unstructured, path ...string) map[string]any {
	m, _, _ := unstructured.NestedMap(u.Object, path...)
	return crossplaneaws.FlattenFields(m)
}

func sortedKeys(maps ...map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range maps {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func isScalar(v any) bool {
	switch v.(type) {
	case string, bool, int64, float64, int:
		return true
	default:
		return false
	}
}

// ActionFor is the planned action of changing a field of an existing
// resource; unset says the resource has no value for it yet. A field not
// classified is treated as immutable: refused rather than guessed. An
// add-only field (secrets encryption) can be set where there is none, and
// is refused otherwise.
func ActionFor(kind, path string, unset bool) cloudv1.PlannedAction {
	class, ok := crossplaneaws.ClassifyField(kind, path)
	switch {
	case class == crossplaneaws.ChangeAddOnly && unset:
		return cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE
	case !ok, class == crossplaneaws.ChangeImmutable, class == crossplaneaws.ChangeAddOnly:
		return cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE
	case class == crossplaneaws.ChangeDisruptive:
		return cloudv1.PlannedAction_PLANNED_ACTION_DISRUPTIVE
	default:
		return cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE
	}
}
