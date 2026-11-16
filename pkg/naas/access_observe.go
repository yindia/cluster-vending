package naas

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/oidc"
)

var ErrAccessBindingsPending = errors.New("waiting for Capsule namespace readiness and generated access bindings")

// ObserveAccessOIDC uses the fresh management reader and actual provider status,
// never a caller-supplied SubTenant status. The platform supports one association;
// replacement/deletion and ambiguous associations fail closed until settled.
func ObserveAccessOIDC(ctx context.Context, reader client.Reader, parent *cloud.Cluster, spec *cloudv1.ClusterSpec) (string, error) {
	p := oidc.Resolve(spec.GetAccess().GetOidc())
	if p == nil || spec.GetAws() == nil || parent.Status.AWS == nil {
		return "", ErrAccessOIDCNotConfigured
	}
	clusterARN, err := arn.Parse(parent.Status.AWS.ClusterARN)
	clusterResource := strings.Split(clusterARN.Resource, "/")
	if err != nil || clusterARN.Service != "eks" || clusterARN.Partition == "" || clusterARN.AccountID == "" || clusterARN.Region == "" ||
		clusterARN.Region != spec.Region || len(clusterResource) != 2 || clusterResource[0] != "cluster" || clusterResource[1] == "" {
		return "", fmt.Errorf("waiting for the observed physical EKS cluster identity")
	}
	clusterName := clusterResource[1]
	list := &awseks.IdentityProviderConfigList{}
	if err := reader.List(ctx, list, client.InNamespace(parent.Namespace), client.MatchingLabels(crossplane.ClusterLabels(parent.Labels[cloudstore.LabelTenant], parent.Labels[cloudstore.LabelID]))); err != nil {
		return "", fmt.Errorf("read EKS OIDC associations: %w", err)
	}
	if len(list.Items) != 1 || list.Continue != "" {
		return "", fmt.Errorf("waiting for exactly one current EKS OIDC association")
	}
	association := &list.Items[0]
	parameters, observed := association.Spec.ForProvider, association.Status.AtProvider
	associationARN, err := arn.Parse(ptr.Deref(observed.Arn, ""))
	associationResource := strings.Split(associationARN.Resource, "/")
	if err != nil || associationARN.Service != "eks" || associationARN.Partition != clusterARN.Partition || associationARN.AccountID != clusterARN.AccountID || associationARN.Region != clusterARN.Region ||
		len(associationResource) != 5 || associationResource[0] != "identityproviderconfig" || associationResource[1] != clusterName || associationResource[2] != "oidc" || associationResource[3] == "" || associationResource[4] == "" {
		return "", fmt.Errorf("observed OIDC association ARN does not match the physical EKS partition, account, region and cluster")
	}
	prefix := crossplaneaws.ClusterPrefix(parent.Labels[cloudstore.LabelTenant], parent.Labels[cloudstore.LabelID], parent.Name)
	bound := parameters.ClusterNameRef != nil && parameters.ClusterNameRef.Name == prefix && (parameters.ClusterNameRef.Namespace == "" || parameters.ClusterNameRef.Namespace == parent.Namespace)
	if !bound {
		bound = parameters.ClusterNameRef == nil && ptr.Deref(parameters.ClusterName, "") == clusterName
	}
	if association.UID == "" || !association.DeletionTimestamp.IsZero() || !bound ||
		ptr.Deref(parameters.Region, "") != spec.Region || ptr.Deref(observed.ClusterName, "") != clusterName ||
		ptr.Deref(observed.Status, "") != "ACTIVE" || association.GetCondition(xpv2.TypeReady).Status != corev1.ConditionTrue || association.GetCondition(xpv2.TypeSynced).Status != corev1.ConditionTrue {
		return "", fmt.Errorf("waiting for a Ready, Synced, ACTIVE OIDC association bound to this EKS cluster")
	}
	if parameters.Oidc == nil || observed.Oidc == nil {
		return "", fmt.Errorf("waiting for observed EKS OIDC claim mapping")
	}
	claims := func(values map[string]*string) map[string]string {
		out := map[string]string{}
		for key, value := range values {
			out[key] = ptr.Deref(value, "")
		}
		return out
	}
	// EKS documents only these username defaults: sub; issuerURL# except
	// for email; and '-' explicitly disables prefixing. Group mapping is
	// not normalized. See API_OidcIdentityProviderConfigRequest in AWS docs.
	username := func(claim, prefix string, observed bool) (string, string) {
		if claim == "" {
			claim = "sub"
		}
		if prefix == "-" || observed && prefix == "" && p.UsernamePrefix == "-" {
			prefix = ""
		} else if prefix == "" && claim != "email" {
			prefix = p.IssuerURL + "#"
		}
		return claim, prefix
	}
	expectedClaim, expectedPrefix := username(p.UsernameClaim, p.UsernamePrefix, false)
	matches := func(issuer, audience, groupClaim, groupPrefix, userClaim, userPrefix *string, required map[string]*string, observed bool) bool {
		claim, prefix := username(ptr.Deref(userClaim, ""), ptr.Deref(userPrefix, ""), observed)
		return ptr.Deref(issuer, "") == p.IssuerURL && ptr.Deref(audience, "") == p.ClientID &&
			ptr.Deref(groupClaim, "") == p.GroupsClaim && ptr.Deref(groupPrefix, "") == p.GroupsPrefix &&
			claim == expectedClaim && prefix == expectedPrefix && maps.Equal(claims(required), p.RequiredClaims)
	}
	want, actual := parameters.Oidc, observed.Oidc
	if !matches(want.IssuerURL, want.ClientID, want.GroupsClaim, want.GroupsPrefix, want.UsernameClaim, want.UsernamePrefix, want.RequiredClaims, false) ||
		!matches(actual.IssuerURL, actual.ClientID, actual.GroupsClaim, actual.GroupsPrefix, actual.UsernameClaim, actual.UsernamePrefix, actual.RequiredClaims, true) {
		return "", fmt.Errorf("observed EKS OIDC mapping does not match current desired issuer, audience and claims")
	}
	return string(association.UID), nil
}

// TenantProvisioned requires Capsule to acknowledge its current generation.
func TenantProvisioned(tenant *unstructured.Unstructured) bool {
	if tenant == nil || tenant.GetUID() == "" || tenant.GetGeneration() < 1 {
		return false
	}
	observed, _, _ := unstructured.NestedInt64(tenant.Object, "status", "observedGeneration")
	conditions, _, _ := unstructured.NestedSlice(tenant.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if ok && condition["type"] == "Ready" && condition["status"] == "True" && observed >= tenant.GetGeneration() {
			return true
		}
	}
	return false
}

// ProvisioningPayloadMatches separates the generated access-ready label from
// baseline configuration. Its asynchronous propagation is verified separately
// with the access bindings, so opening access cannot flap baseline readiness.
func ProvisioningPayloadMatches(current, expected *unstructured.Unstructured) bool {
	_, readinessOwned := expected.GetLabels()["cloud.cluster-vending.io/naas-ready"]
	if expected.GetKind() == "Namespace" && current.GetKind() == "Namespace" && readinessOwned {
		current = current.DeepCopy()
		labels := current.GetLabels()
		if labels["cloud.cluster-vending.io/naas-ready"] != "true" && labels["cloud.cluster-vending.io/naas-ready"] != "false" {
			return false
		}
		labels["cloud.cluster-vending.io/naas-ready"] = expected.GetLabels()["cloud.cluster-vending.io/naas-ready"]
		current.SetLabels(labels)
	}
	return payloadMatches(current, expected)
}

// ObserveAccessBindings checks every owned namespace, detects unexpected RBAC,
// and distinguishes expected replication lag from mismatched/extra grants.
func ObserveAccessBindings(ctx context.Context, workload client.Client, sub *cloud.SubTenant, remote *unstructured.Unstructured, access *ResolvedAccess) error {
	pending := false
	for _, key := range sub.Spec.NamespaceKeys {
		namespace := &corev1.Namespace{}
		if err := workload.Get(ctx, client.ObjectKey{Name: remote.GetName() + "-" + string(key)}, namespace); err != nil {
			return err
		}
		if !OwnTenant(remote, sub) || namespace.UID == "" || !namespace.DeletionTimestamp.IsZero() || namespace.Labels[LabelSubTenantID] != sub.Name ||
			namespace.Labels[LabelTenant] != sub.Spec.TenantRef.Name || namespace.Labels["capsule.clastix.io/tenant"] != remote.GetName() ||
			!slices.ContainsFunc(namespace.OwnerReferences, func(owner metav1.OwnerReference) bool { return tenantOwner(owner, remote) }) {
			return fmt.Errorf("namespace access ownership is not current")
		}
		if namespace.Labels["cloud.cluster-vending.io/naas-ready"] != "true" {
			pending = true
		}
		expected := map[string]GroupBinding{}
		for _, binding := range access.Bindings {
			expected["naas-access-"+binding.ID] = binding
		}
		list := &rbacv1.RoleBindingList{}
		if err := workload.List(ctx, list, client.InNamespace(namespace.Name)); err != nil {
			return err
		}
		if list.Continue != "" {
			return fmt.Errorf("namespace access inventory is incomplete")
		}
		for _, binding := range list.Items {
			wanted, exists := expected[binding.Name]
			if !exists {
				raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&binding)
				if err != nil || !platformOwnerBinding(&unstructured.Unstructured{Object: raw}, remote) {
					return fmt.Errorf("unexpected namespace RoleBinding %s/%s", namespace.Name, binding.Name)
				}
				continue
			}
			if !binding.DeletionTimestamp.IsZero() || binding.Labels[LabelSubTenantID] != sub.Name ||
				binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: wanted.Role}) ||
				!reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "Group", Name: wanted.Group}}) {
				return fmt.Errorf("namespace RoleBinding %s/%s differs from approved group access", namespace.Name, binding.Name)
			}
			delete(expected, binding.Name)
		}
		if len(expected) != 0 {
			pending = true
		}
	}
	if pending {
		return ErrAccessBindingsPending
	}
	return nil
}
