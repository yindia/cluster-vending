package naas

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

// TenantProvisioningReady is the per-allocation check: the current Tenant is
// Ready and every namespace exists with its UID and BaselineReady.
func TenantProvisioningReady(sub *cloud.SubTenant, remote *unstructured.Unstructured, inventory []cloud.SubTenantNamespaceStatus) bool {
	if !TenantProvisioned(remote) || len(inventory) != len(sub.Spec.NamespaceKeys) {
		return false
	}
	for _, key := range sub.Spec.NamespaceKeys {
		found := false
		for _, namespace := range inventory {
			if namespace.Key == key && namespace.Name == remote.GetName()+"-"+string(key) && namespace.UID != "" && meta.IsStatusConditionTrue(namespace.Conditions, "BaselineReady") {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
