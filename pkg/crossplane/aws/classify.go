package aws

import (
	"fmt"
	"sort"
	"strings"
)

// ChangeClass is what changing a field of an existing managed resource
// does to the cloud resource.
type ChangeClass int

// Change classes.
const (
	// ChangeInPlace: updated on the existing resource, no interruption.
	ChangeInPlace ChangeClass = iota + 1
	// ChangeDisruptive: updated in place, but workloads are affected while
	// it rolls out (control plane upgrade, node replacement, add-on
	// restart).
	ChangeDisruptive
	// ChangeImmutable: the cloud cannot change it; the resource would have
	// to be replaced.
	ChangeImmutable
	// ChangeAddOnly: can be set on an existing resource that has none (in
	// place), never changed or removed afterwards (EKS secrets
	// encryption).
	ChangeAddOnly
)

// fieldClasses classifies the spec.forProvider fields of every kind the
// cluster renderer outputs, by path prefix (the longest matching prefix
// wins). Immutable is what the Terraform AWS provider marks ForceNew for
// the kind's resource (aws_eks_cluster, aws_eks_node_group, aws_eks_addon,
// aws_iam_role, aws_iam_role_policy_attachment, aws_iam_policy,
// aws_eks_pod_identity_association, aws_eks_access_entry,
// aws_eks_access_policy_association); disruptive is what EKS rolls out by
// replacing or restarting something. TestEveryRenderedFieldIsClassified
// fails when the renderer outputs a field missing here.
var fieldClasses = map[string]map[string]ChangeClass{
	"Cluster": {
		"region": ChangeImmutable, "roleArn": ChangeImmutable,
		"version":                                              ChangeDisruptive,
		"vpcConfig.subnetIds":                                  ChangeInPlace,
		"vpcConfig.endpointPrivateAccess":                      ChangeInPlace,
		"vpcConfig.endpointPublicAccess":                       ChangeInPlace,
		"vpcConfig.publicAccessCidrs":                          ChangeInPlace,
		"vpcConfig.securityGroupIds":                           ChangeImmutable,
		"encryptionConfig":                                     ChangeAddOnly,
		"accessConfig.authenticationMode":                      ChangeInPlace,
		"accessConfig.bootstrapClusterCreatorAdminPermissions": ChangeImmutable,
		"enabledClusterLogTypes":                               ChangeInPlace,
		"upgradePolicy":                                        ChangeInPlace,
		"kubernetesNetworkConfig":                              ChangeImmutable,
		"tags":                                                 ChangeInPlace,
	},
	"NodeGroup": {
		"region": ChangeImmutable, "clusterName": ChangeImmutable, "nodeRoleArn": ChangeImmutable,
		"subnetIds": ChangeImmutable, "instanceTypes": ChangeImmutable, "capacityType": ChangeImmutable,
		"amiType": ChangeImmutable, "diskSize": ChangeImmutable,
		"launchTemplate": ChangeImmutable, "launchTemplate.version": ChangeDisruptive,
		"scalingConfig": ChangeInPlace, "labels": ChangeInPlace, "taint": ChangeInPlace, "tags": ChangeInPlace,
		"version": ChangeDisruptive,
	},
	"Addon": {
		"region": ChangeImmutable, "clusterName": ChangeImmutable, "addonName": ChangeImmutable,
		"addonVersion": ChangeDisruptive, "configurationValues": ChangeDisruptive,
		"resolveConflictsOnCreate": ChangeInPlace, "resolveConflictsOnUpdate": ChangeInPlace, "tags": ChangeInPlace,
	},
	"Role":  {"assumeRolePolicy": ChangeInPlace, "tags": ChangeInPlace},
	"Group": {"region": ChangeImmutable, "retentionInDays": ChangeInPlace, "tags": ChangeInPlace},
	"Key": {
		"region": ChangeImmutable, "description": ChangeInPlace, "enableKeyRotation": ChangeInPlace, "tags": ChangeInPlace,
	},
	"Alias":                 {"region": ChangeImmutable, "targetKeyId": ChangeInPlace},
	"OpenIDConnectProvider": {"url": ChangeImmutable, "clientIdList": ChangeInPlace, "thumbprintList": ChangeInPlace, "tags": ChangeInPlace},
	// A rendered launch template is never changed: a change renders a new
	// one (and a new node group).
	"LaunchTemplate": {
		"region": ChangeImmutable, "name": ChangeImmutable, "description": ChangeInPlace, "imageId": ChangeImmutable,
		"userData": ChangeImmutable, "vpcSecurityGroupIds": ChangeImmutable, "tags": ChangeInPlace,
		"tagSpecifications": ChangeInPlace,
	},
	"RolePolicyAttachment": {"policyArn": ChangeImmutable, "role": ChangeImmutable},
	"Policy":               {"policy": ChangeInPlace, "description": ChangeImmutable, "tags": ChangeInPlace},
	"PodIdentityAssociation": {
		"region": ChangeImmutable, "clusterName": ChangeImmutable, "namespace": ChangeImmutable,
		"serviceAccount": ChangeImmutable, "roleArn": ChangeInPlace, "tags": ChangeInPlace,
	},
	"AccessEntry": {
		"region": ChangeImmutable, "clusterName": ChangeImmutable, "principalArn": ChangeImmutable,
		"type": ChangeImmutable, "tags": ChangeInPlace,
	},
	"AccessPolicyAssociation": {
		"region": ChangeImmutable, "clusterName": ChangeImmutable, "principalArn": ChangeImmutable,
		"policyArn": ChangeImmutable, "accessScope": ChangeImmutable,
	},
	// EKS cannot change an association: a change renders a new object.
	"IdentityProviderConfig": {
		"region": ChangeImmutable, "clusterName": ChangeImmutable, "oidc": ChangeImmutable, "tags": ChangeInPlace,
	},
	"ClusterAuth": {"region": ChangeImmutable, "clusterName": ChangeImmutable, "refreshPeriod": ChangeInPlace},
	// The event bus of a rule or target is add-only: an earlier build left
	// it unset, which means "default" (the provider cannot read the object
	// without it), so setting it on an existing object is no replacement.
	// Karpenter's interruption queue and its EventBridge rules
	// (aws_sqs_queue, aws_sqs_queue_policy, aws_cloudwatch_event_rule,
	// aws_cloudwatch_event_target): tags change in place (TagQueue,
	// TagResource), names do not.
	"Queue": {
		"region": ChangeImmutable, "name": ChangeImmutable, "fifoQueue": ChangeImmutable,
		"messageRetentionSeconds": ChangeInPlace, "sqsManagedSseEnabled": ChangeInPlace, "tags": ChangeInPlace,
	},
	"QueuePolicy": {"region": ChangeImmutable, "queueUrl": ChangeImmutable, "policy": ChangeInPlace},
	"Rule": {
		"region": ChangeImmutable, "name": ChangeImmutable, "eventBusName": ChangeAddOnly,
		"description": ChangeInPlace, "eventPattern": ChangeInPlace, "state": ChangeInPlace, "tags": ChangeInPlace,
	},
	"Target": {
		"region": ChangeImmutable, "rule": ChangeImmutable, "targetId": ChangeImmutable, "eventBusName": ChangeAddOnly,
		"arn": ChangeInPlace,
	},
	// The results bucket and its settings (aws_s3_bucket and
	// aws_s3_bucket_public_access_block, _ownership_controls,
	// _server_side_encryption_configuration, _policy,
	// _lifecycle_configuration). The bucket's name is its external name, not
	// a field. Terraform marks the region, the object lock and the bucket
	// of each setting ForceNew; tags and the settings' content change in
	// place.
	"Bucket": {"region": ChangeImmutable, "objectLockEnabled": ChangeImmutable, "tags": ChangeInPlace},
	"BucketPublicAccessBlock": {
		"region": ChangeImmutable, "blockPublicAcls": ChangeInPlace, "blockPublicPolicy": ChangeInPlace,
		"ignorePublicAcls": ChangeInPlace, "restrictPublicBuckets": ChangeInPlace,
	},
	"BucketOwnershipControls":                 {"region": ChangeImmutable, "rule": ChangeInPlace},
	"BucketServerSideEncryptionConfiguration": {"region": ChangeImmutable, "rule": ChangeInPlace},
	"BucketPolicy":                            {"region": ChangeImmutable, "policy": ChangeInPlace},
	"BucketLifecycleConfiguration":            {"region": ChangeImmutable, "rule": ChangeInPlace},
}

// ClassifyField is the class of a kind's spec.forProvider field (path
// without "forProvider."); false when the kind or field is not classified.
func ClassifyField(kind, path string) (ChangeClass, bool) {
	fields, ok := fieldClasses[kind]
	if !ok {
		return 0, false
	}
	for p := path; p != ""; {
		if c, ok := fields[p]; ok {
			return c, true
		}
		i := strings.LastIndex(p, ".")
		if i < 0 {
			break
		}
		p = p[:i]
	}
	return 0, false
}

// FlattenFields returns the leaves of a spec.forProvider (or
// status.atProvider) map by dotted path. Lists are leaves (compared as a
// whole). Left out, they never differ between what the platform renders
// and what it would show a client: references and selectors (resolved by
// Crossplane into the field next to them) and the crossplane-* tags the
// provider adds.
func FlattenFields(m map[string]any) map[string]any {
	out := map[string]any{}
	flatten("", m, out)
	return out
}

func flatten(prefix string, m map[string]any, out map[string]any) {
	for k, v := range m {
		if hiddenField(prefix, k) {
			continue
		}
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok {
			flatten(path, child, out)
			continue
		}
		out[path] = v
	}
}

func hiddenField(prefix, key string) bool {
	if strings.HasSuffix(key, "Ref") || strings.HasSuffix(key, "Refs") || strings.HasSuffix(key, "Selector") {
		return true
	}
	return (prefix == "tags" || strings.HasSuffix(prefix, ".tags")) && strings.HasPrefix(key, "crossplane-")
}

// FieldString renders a field value for diffs: scalars as they are, lists
// and other values compactly, sorted where order carries no meaning.
func FieldString(v any) string {
	switch t := v.(type) {
	case nil:
		return "<unset>"
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, FieldString(e))
		}
		sort.Strings(parts)
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprint(t)
	}
}
