package deprecation

import "strings"

// API groups the table names more than once.
const (
	groupExtensions    = "extensions/v1beta1"
	groupAppsV1        = "apps/v1"
	groupNetworkingV1  = "networking.k8s.io/v1"
	groupRBACV1        = "rbac.authorization.k8s.io/v1"
	groupStorageV1     = "storage.k8s.io/v1"
	groupStorageBeta   = "storage.k8s.io/v1beta1"
	groupAuthzV1       = "authorization.k8s.io/v1"
	groupAuthzBeta     = "authorization.k8s.io/v1beta1"
	groupAdmissionV1   = "admissionregistration.k8s.io/v1"
	groupAdmissionBeta = "admissionregistration.k8s.io/v1beta1"
	groupFlowV1        = "flowcontrol.apiserver.k8s.io/v1"
	groupAutoscalingV2 = "autoscaling/v2"
)

// rulesTable is the versions Kubernetes has stopped serving, from the
// "Deprecated API Migration Guide". It is read-only after package
// initialisation.
var rulesTable = []Rule{
	// Removed in 1.16.
	{APIVersion: groupExtensions, Kind: "Deployment", Replacement: groupAppsV1, DeprecatedIn: 9, RemovedIn: 16},
	{APIVersion: "apps/v1beta1", Kind: "Deployment", Replacement: groupAppsV1, DeprecatedIn: 9, RemovedIn: 16},
	{APIVersion: "apps/v1beta2", Kind: "Deployment", Replacement: groupAppsV1, DeprecatedIn: 9, RemovedIn: 16},
	{APIVersion: groupExtensions, Kind: "DaemonSet", Replacement: groupAppsV1, DeprecatedIn: 9, RemovedIn: 16},
	{APIVersion: "apps/v1beta2", Kind: "DaemonSet", Replacement: groupAppsV1, DeprecatedIn: 9, RemovedIn: 16},
	{APIVersion: "apps/v1beta1", Kind: "StatefulSet", Replacement: groupAppsV1, DeprecatedIn: 9, RemovedIn: 16},
	{APIVersion: "apps/v1beta2", Kind: "StatefulSet", Replacement: groupAppsV1, DeprecatedIn: 9, RemovedIn: 16},
	{APIVersion: groupExtensions, Kind: "ReplicaSet", Replacement: groupAppsV1, DeprecatedIn: 9, RemovedIn: 16},
	{APIVersion: "apps/v1beta2", Kind: "ReplicaSet", Replacement: groupAppsV1, DeprecatedIn: 9, RemovedIn: 16},
	{APIVersion: groupExtensions, Kind: "NetworkPolicy", Replacement: groupNetworkingV1, DeprecatedIn: 9, RemovedIn: 16},
	{APIVersion: groupExtensions, Kind: "PodSecurityPolicy", Replacement: "policy/v1beta1", DeprecatedIn: 10, RemovedIn: 16},

	// Removed in 1.22.
	{APIVersion: groupAdmissionBeta, Kind: "MutatingWebhookConfiguration", Replacement: groupAdmissionV1, DeprecatedIn: 16, RemovedIn: 22},
	{APIVersion: groupAdmissionBeta, Kind: "ValidatingWebhookConfiguration", Replacement: groupAdmissionV1, DeprecatedIn: 16, RemovedIn: 22},
	{APIVersion: "apiextensions.k8s.io/v1beta1", Kind: "CustomResourceDefinition", Replacement: "apiextensions.k8s.io/v1", DeprecatedIn: 16, RemovedIn: 22},
	{APIVersion: "apiregistration.k8s.io/v1beta1", Kind: "APIService", Replacement: "apiregistration.k8s.io/v1", DeprecatedIn: 19, RemovedIn: 22},
	{APIVersion: "authentication.k8s.io/v1beta1", Kind: "TokenReview", Replacement: "authentication.k8s.io/v1", DeprecatedIn: 19, RemovedIn: 22},
	{APIVersion: groupAuthzBeta, Kind: "SubjectAccessReview", Replacement: groupAuthzV1, DeprecatedIn: 19, RemovedIn: 22},
	{APIVersion: groupAuthzBeta, Kind: "LocalSubjectAccessReview", Replacement: groupAuthzV1, DeprecatedIn: 19, RemovedIn: 22},
	{APIVersion: groupAuthzBeta, Kind: "SelfSubjectAccessReview", Replacement: groupAuthzV1, DeprecatedIn: 19, RemovedIn: 22},
	{APIVersion: "certificates.k8s.io/v1beta1", Kind: "CertificateSigningRequest", Replacement: "certificates.k8s.io/v1", DeprecatedIn: 19, RemovedIn: 22},
	{APIVersion: "coordination.k8s.io/v1beta1", Kind: "Lease", Replacement: "coordination.k8s.io/v1", DeprecatedIn: 19, RemovedIn: 22},
	{APIVersion: groupExtensions, Kind: "Ingress", Replacement: groupNetworkingV1, DeprecatedIn: 14, RemovedIn: 22},
	{APIVersion: "networking.k8s.io/v1beta1", Kind: "Ingress", Replacement: groupNetworkingV1, DeprecatedIn: 19, RemovedIn: 22},
	{APIVersion: "networking.k8s.io/v1beta1", Kind: "IngressClass", Replacement: groupNetworkingV1, DeprecatedIn: 19, RemovedIn: 22},
	{APIVersion: "rbac.authorization.k8s.io/v1beta1", Kind: "ClusterRole", Replacement: groupRBACV1, DeprecatedIn: 17, RemovedIn: 22},
	{APIVersion: "rbac.authorization.k8s.io/v1beta1", Kind: "ClusterRoleBinding", Replacement: groupRBACV1, DeprecatedIn: 17, RemovedIn: 22},
	{APIVersion: "rbac.authorization.k8s.io/v1beta1", Kind: "Role", Replacement: groupRBACV1, DeprecatedIn: 17, RemovedIn: 22},
	{APIVersion: "rbac.authorization.k8s.io/v1beta1", Kind: "RoleBinding", Replacement: groupRBACV1, DeprecatedIn: 17, RemovedIn: 22},
	{APIVersion: "rbac.authorization.k8s.io/v1alpha1", Kind: "ClusterRole", Replacement: groupRBACV1, DeprecatedIn: 17, RemovedIn: 22},
	{APIVersion: "rbac.authorization.k8s.io/v1alpha1", Kind: "ClusterRoleBinding", Replacement: groupRBACV1, DeprecatedIn: 17, RemovedIn: 22},
	{APIVersion: "rbac.authorization.k8s.io/v1alpha1", Kind: "Role", Replacement: groupRBACV1, DeprecatedIn: 17, RemovedIn: 22},
	{APIVersion: "rbac.authorization.k8s.io/v1alpha1", Kind: "RoleBinding", Replacement: groupRBACV1, DeprecatedIn: 17, RemovedIn: 22},
	{APIVersion: "scheduling.k8s.io/v1beta1", Kind: "PriorityClass", Replacement: "scheduling.k8s.io/v1", DeprecatedIn: 14, RemovedIn: 22},
	{APIVersion: groupStorageBeta, Kind: "CSIDriver", Replacement: groupStorageV1, DeprecatedIn: 19, RemovedIn: 22},
	{APIVersion: groupStorageBeta, Kind: "CSINode", Replacement: groupStorageV1, DeprecatedIn: 17, RemovedIn: 22},
	{APIVersion: groupStorageBeta, Kind: "StorageClass", Replacement: groupStorageV1, DeprecatedIn: 19, RemovedIn: 22},
	{APIVersion: groupStorageBeta, Kind: "VolumeAttachment", Replacement: groupStorageV1, DeprecatedIn: 19, RemovedIn: 22},

	// Removed in 1.25.
	{APIVersion: "batch/v1beta1", Kind: "CronJob", Replacement: "batch/v1", DeprecatedIn: 21, RemovedIn: 25},
	{APIVersion: "discovery.k8s.io/v1beta1", Kind: "EndpointSlice", Replacement: "discovery.k8s.io/v1", DeprecatedIn: 21, RemovedIn: 25},
	{APIVersion: "events.k8s.io/v1beta1", Kind: "Event", Replacement: "events.k8s.io/v1", DeprecatedIn: 19, RemovedIn: 25},
	{APIVersion: "autoscaling/v2beta1", Kind: "HorizontalPodAutoscaler", Replacement: groupAutoscalingV2, DeprecatedIn: 22, RemovedIn: 25},
	{APIVersion: "policy/v1beta1", Kind: "PodDisruptionBudget", Replacement: "policy/v1", DeprecatedIn: 21, RemovedIn: 25},
	{APIVersion: "policy/v1beta1", Kind: "PodSecurityPolicy", Replacement: "", DeprecatedIn: 21, RemovedIn: 25},
	{APIVersion: "node.k8s.io/v1beta1", Kind: "RuntimeClass", Replacement: "node.k8s.io/v1", DeprecatedIn: 20, RemovedIn: 25},

	// Removed in 1.26.
	{APIVersion: "flowcontrol.apiserver.k8s.io/v1beta1", Kind: "FlowSchema", Replacement: groupFlowV1, DeprecatedIn: 23, RemovedIn: 26},
	{APIVersion: "flowcontrol.apiserver.k8s.io/v1beta1", Kind: "PriorityLevelConfiguration", Replacement: groupFlowV1, DeprecatedIn: 23, RemovedIn: 26},
	{APIVersion: "autoscaling/v2beta2", Kind: "HorizontalPodAutoscaler", Replacement: groupAutoscalingV2, DeprecatedIn: 23, RemovedIn: 26},

	// Removed in 1.27.
	{APIVersion: groupStorageBeta, Kind: "CSIStorageCapacity", Replacement: groupStorageV1, DeprecatedIn: 24, RemovedIn: 27},

	// Removed in 1.29.
	{APIVersion: "flowcontrol.apiserver.k8s.io/v1beta2", Kind: "FlowSchema", Replacement: groupFlowV1, DeprecatedIn: 26, RemovedIn: 29},
	{APIVersion: "flowcontrol.apiserver.k8s.io/v1beta2", Kind: "PriorityLevelConfiguration", Replacement: groupFlowV1, DeprecatedIn: 26, RemovedIn: 29},

	// Removed in 1.32.
	{APIVersion: "flowcontrol.apiserver.k8s.io/v1beta3", Kind: "FlowSchema", Replacement: groupFlowV1, DeprecatedIn: 29, RemovedIn: 32},
	{APIVersion: "flowcontrol.apiserver.k8s.io/v1beta3", Kind: "PriorityLevelConfiguration", Replacement: groupFlowV1, DeprecatedIn: 29, RemovedIn: 32},
}

// ruleKey identifies a rule: a deprecated apiVersion of a kind.
type ruleKey struct {
	apiVersion string
	kind       string
}

// groupKind is a kind in an API group; the group is empty for the core
// group.
type groupKind struct {
	group string
	kind  string
}

// ruleIndex and ruleGroupKinds are built once from rulesTable and only
// read afterwards.
var (
	ruleIndex      = indexRules(rulesTable)
	ruleGroupKinds = groupKindsOf(rulesTable)
)

// Rules is the table of deprecated versions. The slice is a copy: changing
// it does not change what Match answers.
func Rules() []Rule {
	out := make([]Rule, len(rulesTable))
	copy(out, rulesTable)
	return out
}

// Match finds the rule for an object written as apiVersion and kind. Both
// are compared exactly.
func Match(apiVersion, kind string) (Rule, bool) {
	rule, ok := ruleIndex[ruleKey{apiVersion: apiVersion, kind: kind}]
	return rule, ok
}

// servesRuleKind reports whether a kind served in group may hold objects
// written with a deprecated version: the group is that of a rule for the
// kind, or of the rule's replacement (Ingress moved from extensions to
// networking.k8s.io).
func servesRuleKind(group, kind string) bool {
	_, ok := ruleGroupKinds[groupKind{group: group, kind: kind}]
	return ok
}

func indexRules(rules []Rule) map[ruleKey]Rule {
	index := make(map[ruleKey]Rule, len(rules))
	for _, rule := range rules {
		index[ruleKey{apiVersion: rule.APIVersion, kind: rule.Kind}] = rule
	}
	return index
}

func groupKindsOf(rules []Rule) map[groupKind]struct{} {
	set := make(map[groupKind]struct{}, len(rules))
	for _, rule := range rules {
		set[groupKind{group: groupOf(rule.APIVersion), kind: rule.Kind}] = struct{}{}
		if rule.Replacement != "" {
			set[groupKind{group: groupOf(rule.Replacement), kind: rule.Kind}] = struct{}{}
		}
	}
	return set
}

// groupOf is the API group of an apiVersion: "apps" of "apps/v1", empty for
// the core group's "v1".
func groupOf(apiVersion string) string {
	group, _, found := strings.Cut(apiVersion, "/")
	if !found {
		return ""
	}
	return group
}
