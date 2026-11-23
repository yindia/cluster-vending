package aws

import (
	"strings"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	flowcontrolv1 "k8s.io/api/flowcontrol/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The live reader's identity (PROP-02 7.1). ClusterAuth makes no Kubernetes
// identity: it writes a kubeconfig whose token is an EKS token of its
// ProviderConfig's AWS identity. The live identity is therefore the account's
// reader role, assumed by a second ProviderConfig, mapped by its own access
// entry (no access policy) to a Kubernetes group that a read-only ClusterRole
// is bound to in the workload cluster.
const (
	// LiveReaderGroup is the Kubernetes group of the live identity.
	LiveReaderGroup = "cluster-vending:live-reader"
	// LiveReaderRole names the ClusterRole, its ClusterRoleBinding, the
	// PriorityLevelConfiguration and the FlowSchema in the workload cluster.
	LiveReaderRole = "cluster-vending-live-reader"
	// LiveFlowSchema names the PriorityLevelConfiguration and FlowSchema that
	// queue the live LISTs in their own level (open question 4).
	LiveFlowSchema = "cluster-vending-live"
	// DefaultReaderRoleName is the reader stack's default RoleName.
	DefaultReaderRoleName = "cluster-vending-reader"
	// liveNominalShares is the level's nominalConcurrencyShares.
	liveNominalShares = 10
	// liveFlowPrecedence matches before the global-default (9900) and the
	// workload levels, after the system ones.
	liveFlowPrecedence = 800
	authModeConfigMap  = "CONFIG_MAP"
)

// LiveProviderConfigName is the account's second ProviderConfig, the one that
// assumes the reader role: "<account's ProviderConfig>-live".
func LiveProviderConfigName(providerConfigName string) string { return providerConfigName + "-live" }

// LiveKubeconfigSecretName is the Secret the live ClusterAuth writes: the only
// Secret the live reader reads.
func LiveKubeconfigSecretName(prefix string) string { return prefix + "-live-kubeconfig" }

// LiveClusterAuthName is the cluster's live ClusterAuth.
func LiveClusterAuthName(prefix string) string { return prefix + "-live-auth" }

// LiveAccessEntryName is the cluster's live access entry.
func LiveAccessEntryName(prefix string) string { return prefix + "-live" }

// ReaderRoleARN is the account's reader role: readerRole when the
// CloudAccount names one (spec.aws.readerRole), else the reader stack's
// default role in the account, in the partition of the tenant role ARN.
func ReaderRoleARN(accountID, tenantRoleARN, readerRole string) string {
	if readerRole != "" {
		return readerRole
	}
	if accountID == "" {
		return ""
	}
	partition := "aws"
	if p, rest, ok := strings.Cut(strings.TrimPrefix(tenantRoleARN, "arn:"), ":"); ok && rest != "" && strings.HasPrefix(tenantRoleARN, "arn:") {
		partition = p
	}
	return "arn:" + partition + ":iam::" + accountID + ":role/" + DefaultReaderRoleName
}

// LiveProviderConfig is the account's ProviderConfig of the reader role: the
// account's own (ProviderConfig), assuming readerRoleARN instead of the
// tenant role. The live ClusterAuths reference it.
func LiveProviderConfig(namespace, providerConfigName, tenantID, readerRoleARN, externalID string, secret *CredentialsSecret) client.Object {
	return ProviderConfig(namespace, LiveProviderConfigName(providerConfigName), tenantID, readerRoleARN, externalID, secret)
}

// HasLiveIdentity: the plan renders the live ClusterAuth and access entry. An
// EKS cluster in CONFIG_MAP mode has no access entries, and the platform does
// not change its mode: it gets no live identity.
func (p ClusterPlan) HasLiveIdentity() bool {
	return p.LiveReaderRoleARN != "" && p.LiveProviderConfigName != "" && !strings.EqualFold(p.AuthenticationMode, authModeConfigMap)
}

// IsLiveIdentity reports whether name is one of the cluster's live identity
// objects. They do not hold the cluster's readiness back: an account without
// the reader stack has a live ClusterAuth that is never Ready, and the
// cluster runs without live reads.
func IsLiveIdentity(prefix, name string) bool {
	return name == LiveClusterAuthName(prefix) || name == LiveAccessEntryName(prefix)
}

// liveObjects are the live ClusterAuth and access entry: the reader role
// mapped to LiveReaderGroup, type STANDARD, with no access policy.
func (b clusterBuilder) liveObjects() []client.Object {
	if !b.p.HasLiveIdentity() {
		return nil
	}
	auth := &awseks.ClusterAuth{
		ObjectMeta: b.meta(LiveClusterAuthName(b.p.Prefix)),
		Spec: awseks.ClusterAuthSpec{
			ManagedResourceSpec: xpv2.ManagedResourceSpec{ProviderConfigReference: ProviderConfigRef(b.p.LiveProviderConfigName),
				WriteConnectionSecretToReference: &xpv2.LocalSecretReference{Name: LiveKubeconfigSecretName(b.p.Prefix)}},
			ForProvider: awseks.ClusterAuthParameters{Region: b.p.Region, ClusterNameRef: b.ref(b.p.Prefix)},
		},
	}
	name := LiveAccessEntryName(b.p.Prefix)
	entry := &awseks.AccessEntry{
		ObjectMeta: b.meta(name),
		Spec: awseks.AccessEntrySpec{ManagedResourceSpec: b.spec(), ForProvider: awseks.AccessEntryParameters{
			Region: ptr.To(b.p.Region), ClusterNameRef: b.ref(b.p.Prefix), PrincipalArn: ptr.To(b.p.LiveReaderRoleARN),
			Type: ptr.To(accessEntryStandard), KubernetesGroups: []*string{ptr.To(LiveReaderGroup)}, Tags: b.tags(name),
		}},
	}
	return []client.Object{entry, auth}
}

// LiveReaderRules are what the live identity may do in the workload cluster:
// get and list the kinds a live read projects, nothing else. No Secrets,
// ConfigMaps, VPAs, pod metrics, pods/exec or pods/log, no write verb, no
// watch in version 1.
func LiveReaderRules() []rbacv1.PolicyRule {
	read := []string{"get", "list"}
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"nodes", "pods", "events"}, Verbs: read},
		{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"}, Verbs: read},
		{APIGroups: []string{"karpenter.sh"}, Resources: []string{"nodeclaims"}, Verbs: read},
		{APIGroups: []string{"wgpolicyk8s.io"}, Resources: []string{"policyreports", "clusterpolicyreports"}, Verbs: read},
		{APIGroups: []string{"openreports.io"}, Resources: []string{"reports", "clusterreports"}, Verbs: read},
		{APIGroups: []string{"constraints.gatekeeper.sh"}, Resources: []string{"*"}, Verbs: read},
		{APIGroups: []string{"metrics.k8s.io"}, Resources: []string{"nodes"}, Verbs: read},
	}
}

// LiveReaderObjects are what the controller applies in each workload cluster,
// with its own kubeconfig, once the cluster is ACTIVE: the read-only
// ClusterRole and its binding to LiveReaderGroup, and the priority level and
// flow schema the live LISTs queue in (limited, 10 shares, queuing on), so
// they neither take the cluster's global-default seats nor are starved by
// them.
func LiveReaderObjects() ([]*unstructured.Unstructured, error) {
	labels := map[string]string{"app.kubernetes.io/managed-by": "cluster-vending"}
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Labels: labels} }
	role := &rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: meta(LiveReaderRole), Rules: LiveReaderRules()}
	binding := &rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: meta(LiveReaderRole),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: LiveReaderRole},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: LiveReaderGroup}}}
	level := &flowcontrolv1.PriorityLevelConfiguration{TypeMeta: metav1.TypeMeta{APIVersion: "flowcontrol.apiserver.k8s.io/v1", Kind: "PriorityLevelConfiguration"},
		ObjectMeta: meta(LiveFlowSchema),
		Spec: flowcontrolv1.PriorityLevelConfigurationSpec{Type: flowcontrolv1.PriorityLevelEnablementLimited,
			Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{NominalConcurrencyShares: ptr.To[int32](liveNominalShares),
				LimitResponse: flowcontrolv1.LimitResponse{Type: flowcontrolv1.LimitResponseTypeQueue,
					Queuing: &flowcontrolv1.QueuingConfiguration{Queues: 16, HandSize: 4, QueueLengthLimit: 50}}}}}
	schema := &flowcontrolv1.FlowSchema{TypeMeta: metav1.TypeMeta{APIVersion: "flowcontrol.apiserver.k8s.io/v1", Kind: "FlowSchema"},
		ObjectMeta: meta(LiveFlowSchema),
		Spec: flowcontrolv1.FlowSchemaSpec{
			PriorityLevelConfiguration: flowcontrolv1.PriorityLevelConfigurationReference{Name: LiveFlowSchema},
			MatchingPrecedence:         liveFlowPrecedence,
			DistinguisherMethod:        &flowcontrolv1.FlowDistinguisherMethod{Type: flowcontrolv1.FlowDistinguisherMethodByUserType},
			Rules: []flowcontrolv1.PolicyRulesWithSubjects{{
				Subjects: []flowcontrolv1.Subject{{Kind: flowcontrolv1.SubjectKindGroup, Group: &flowcontrolv1.GroupSubject{Name: LiveReaderGroup}}},
				ResourceRules: []flowcontrolv1.ResourcePolicyRule{{Verbs: []string{"get", "list"}, APIGroups: []string{"*"},
					Resources: []string{"*"}, ClusterScope: true, Namespaces: []string{"*"}}},
			}},
		}}
	out := make([]*unstructured.Unstructured, 0, 4)
	for _, obj := range []runtime.Object{role, binding, level, schema} {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return nil, err
		}
		u := &unstructured.Unstructured{Object: m}
		unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
		out = append(out, u)
	}
	return out, nil
}
