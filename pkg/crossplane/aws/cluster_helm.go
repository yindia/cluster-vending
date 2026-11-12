package aws

import (
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

// HelmProviderConfigGVK is provider-helm's namespaced ProviderConfig
// (crossplane.HelmProviderVersion, helm.m.crossplane.io/v1beta1). The
// provider-helm Go module is not a dependency (it would bring Helm itself
// into the build for one small object), so the object is unstructured; its
// shape is pinned by TestHelmProviderConfig against the CRD's schema.
var HelmProviderConfigGVK = schema.GroupVersionKind{Group: "helm.m.crossplane.io", Version: "v1beta1", Kind: "ProviderConfig"}

// HelmProviderConfigName is the provider-helm ProviderConfig of a cluster,
// in the tenant namespace: what the cluster's components (Helm Releases)
// install through.
func HelmProviderConfigName(prefix string) string { return prefix + "-helm" }

// helmProviderConfigKind is the ProviderConfig as a Usage refers to it.
var helmProviderConfigKind = UsageRef{APIVersion: HelmProviderConfigGVK.GroupVersion().String(), Kind: HelmProviderConfigGVK.Kind}

// helmProviderConfig reaches the workload cluster with the kubeconfig its
// ClusterAuth writes (KubeconfigSecretName, key KubeconfigSecretKey; the
// token in it is refreshed by the AWS provider). The Secret is in the
// ProviderConfig's own namespace, as the namespaced kind requires.
func (b clusterBuilder) helmProviderConfig() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(HelmProviderConfigGVK)
	meta := b.meta(HelmProviderConfigName(b.p.Prefix))
	u.SetNamespace(meta.Namespace)
	u.SetName(meta.Name)
	u.SetLabels(meta.Labels)
	u.Object["spec"] = map[string]any{
		"credentials": map[string]any{
			"source": "Secret",
			"secretRef": map[string]any{
				"namespace": b.p.Namespace,
				"name":      KubeconfigSecretName(b.p.Prefix),
				"key":       KubeconfigSecretKey,
			},
		},
	}
	return u
}

// isHelmProviderConfig reports whether obj is a cluster's provider-helm
// ProviderConfig.
func isHelmProviderConfig(obj client.Object) bool {
	u, ok := obj.(*unstructured.Unstructured)
	return ok && u.GroupVersionKind() == HelmProviderConfigGVK
}

// afterAuth is what the helm ProviderConfig waits for: the ClusterAuth
// Ready, so the kubeconfig Secret it points at exists.
func (b clusterBuilder) afterAuth() []sequence.Condition {
	auth := &awseks.ClusterAuth{ObjectMeta: metav1.ObjectMeta{Namespace: b.p.Namespace, Name: b.name("auth")}}
	return []sequence.Condition{sequence.Ready(auth)}
}
