package cluster

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/yindia/cluster-vending/pkg/crossplane/planning"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// apiFields maps a managed resource's forProvider field (by prefix) to the
// API field it is rendered from, so a plan names what the client set (the
// default node group's: the platform config's key).
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
	// The node group is the platform's: its settings are the platform
	// config's (nodeGroups.system), named so.
	"NodeGroup": {
		"version":               "spec.control_plane.kubernetes_version",
		"scalingConfig.minSize": "nodeGroups.system.minSize",
		"scalingConfig.maxSize": "nodeGroups.system.minSize",
		"instanceTypes":         "nodeGroups.system.instanceTypes",
		"capacityType":          "nodeGroups.system.capacityType",
		"diskSize":              "nodeGroups.system.diskSizeGiB",
		"amiType":               "nodeGroups.system.amiType",
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
	return strings.Join([]string{c.GetApiVersion(), c.GetNamespace(), c.GetKind(), c.GetName(), c.GetAction().String(), c.GetSpecPath(), c.GetDetail()}, "|")
}

// CreateDetail says why an object a plan creates is created.
func CreateDetail(body *unstructured.Unstructured, replacing bool) string {
	if replacing && body.GetKind() == "NodeGroup" {
		return "created to replace the current node group (a setting EKS cannot change in place changed)"
	}
	return "does not exist yet"
}

// FieldChanges uses the cloud-neutral comparison engine with AWS rules.
func FieldChanges(live, dry *unstructured.Unstructured) []*cloudv1.PlannedChange {
	return planning.FieldChanges(live, dry, planning.Policy{
		Classify: func(kind schema.GroupVersionKind, path string, unset bool) cloudv1.PlannedAction {
			return ActionFor(kind.Kind, path, unset)
		},
		APIField: func(kind schema.GroupVersionKind, path string) string { return APIField(kind.Kind, path) },
	})
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
