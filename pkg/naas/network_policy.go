package naas

import (
	"context"
	"errors"
	"fmt"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

var (
	// ErrNetworkPolicyNotEnforced: the cluster's vpc-cni add-on is missing or
	// neither asks for nor reports NetworkPolicy enforcement, so a
	// sub-tenant's deny-by-default policies isolate nothing.
	ErrNetworkPolicyNotEnforced = errors.New("the cluster's vpc-cni add-on does not enforce NetworkPolicy")
	// ErrNetworkPolicyPending: enforcement is asked for but the add-on is
	// not yet Ready and Synced with it as EKS reports.
	ErrNetworkPolicyPending = errors.New("waiting for the cluster's vpc-cni add-on to be Ready and Synced with NetworkPolicy enforcement")
)

// ObserveNetworkPolicyEnforcement reads the parent cluster's vpc-cni
// add-on (the managed resource, from a fresh management reader) and
// returns nil only when EKS reports its configuration enforcing
// NetworkPolicy and the add-on is Ready and Synced. Sub-tenant isolation
// (the bootstrap's deny-by-default NetworkPolicies) holds only then.
func ObserveNetworkPolicyEnforcement(ctx context.Context, reader client.Reader, parent *cloud.Cluster) error {
	list := &awseks.AddonList{}
	labels := crossplane.ClusterLabels(parent.Labels[cloudstore.LabelTenant], parent.Labels[cloudstore.LabelID])
	if err := reader.List(ctx, list, client.InNamespace(parent.Namespace), client.MatchingLabels(labels)); err != nil {
		return fmt.Errorf("read the EKS add-ons of cluster %s: %w", parent.Name, err)
	}
	for i := range list.Items {
		addon := &list.Items[i]
		if ptr.Deref(addon.Spec.ForProvider.AddonName, "") != crossplaneaws.VPCCNIAddon || !addon.DeletionTimestamp.IsZero() {
			continue
		}
		observed := crossplaneaws.NetworkPolicyEnforced(ptr.Deref(addon.Status.AtProvider.ConfigurationValues, ""))
		asked := crossplaneaws.NetworkPolicyEnforced(ptr.Deref(addon.Spec.ForProvider.ConfigurationValues, ""))
		switch {
		case !observed && !asked:
			return fmt.Errorf("cluster %s: %w (its render predates enforcement; it arrives with the cluster's next render)", parent.Name, ErrNetworkPolicyNotEnforced)
		case !observed || addon.GetCondition(xpv2.TypeReady).Status != corev1.ConditionTrue || addon.GetCondition(xpv2.TypeSynced).Status != corev1.ConditionTrue:
			return fmt.Errorf("cluster %s: %w", parent.Name, ErrNetworkPolicyPending)
		}
		return nil
	}
	return fmt.Errorf("cluster %s has no vpc-cni add-on: %w", parent.Name, ErrNetworkPolicyNotEnforced)
}
