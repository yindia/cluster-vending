// Package owner expresses the ownership chain of the platform's kinds in
// the management cluster: CloudAccount owns Network owns Cluster owns
// Component.
package owner

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// Set makes owner the controller owner of dependent and labels dependent
// with the chain it belongs to (cloudv1alpha1.LabelAccount, LabelNetwork,
// LabelCluster),
// so an owner's dependents are a label selection. The owner reference
// blocks the owner's deletion while the dependent exists. The pairs are a
// CloudAccount and one of its Networks, a Network and one of its
// Clusters, or a Cluster and one of its Components (named
// <cluster>-<component>), in the same namespace and matching the dependent's spec
// reference; anything else is an error. It changes dependent in memory
// only.
func Set(owner, dependent client.Object, scheme *runtime.Scheme) error {
	if owner.GetNamespace() != dependent.GetNamespace() {
		return fmt.Errorf("%s %s/%s cannot own %s/%s: an owner and its dependents share a namespace",
			kindOf(owner), owner.GetNamespace(), owner.GetName(), dependent.GetNamespace(), dependent.GetName())
	}
	labels := map[string]string{}
	switch d := dependent.(type) {
	case *cloudv1alpha1.Network:
		account, ok := owner.(*cloudv1alpha1.CloudAccount)
		if !ok || d.Spec.AccountRef.Name != account.Name {
			return mismatch(owner, dependent, "accountRef "+d.Spec.AccountRef.Name)
		}
		labels[cloudv1alpha1.LabelAccount] = account.Name
	case *cloudv1alpha1.Cluster:
		network, ok := owner.(*cloudv1alpha1.Network)
		if !ok || d.Spec.Network.NetworkRef.Name != network.Name {
			return mismatch(owner, dependent, "networkRef "+d.Spec.Network.NetworkRef.Name)
		}
		labels[cloudv1alpha1.LabelNetwork] = network.Name
		labels[cloudv1alpha1.LabelAccount] = network.Spec.AccountRef.Name
	case *cloudv1alpha1.Component:
		cluster, ok := owner.(*cloudv1alpha1.Cluster)
		if !ok || d.Name != cluster.Name+"-"+d.Spec.Component {
			return mismatch(owner, dependent, "component "+d.Spec.Component)
		}
		labels[cloudv1alpha1.LabelCluster] = cluster.Name
		labels[cloudv1alpha1.LabelNetwork] = cluster.Spec.Network.NetworkRef.Name
		if account := cluster.Labels[cloudv1alpha1.LabelAccount]; account != "" {
			labels[cloudv1alpha1.LabelAccount] = account
		}
	default:
		return fmt.Errorf("%T has no owner in this API", dependent)
	}
	if err := controllerutil.SetControllerReference(owner, dependent, scheme,
		controllerutil.WithBlockOwnerDeletion(true)); err != nil {
		return fmt.Errorf("own %s %s: %w", kindOf(dependent), dependent.GetName(), err)
	}
	merged := dependent.GetLabels()
	if merged == nil {
		merged = map[string]string{}
	}
	for k, v := range labels {
		merged[k] = v
	}
	dependent.SetLabels(merged)
	return nil
}

func mismatch(owner, dependent client.Object, ref string) error {
	return fmt.Errorf("%s %s is not the owner of %s %s (its spec names %s)",
		kindOf(owner), owner.GetName(), kindOf(dependent), dependent.GetName(), ref)
}

func kindOf(obj client.Object) string {
	switch obj.(type) {
	case *cloudv1alpha1.CloudAccount:
		return "CloudAccount"
	case *cloudv1alpha1.Network:
		return "Network"
	case *cloudv1alpha1.Cluster:
		return "Cluster"
	case *cloudv1alpha1.Component:
		return "Component"
	}
	return fmt.Sprintf("%T", obj)
}
