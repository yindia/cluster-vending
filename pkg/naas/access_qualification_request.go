package naas

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

// ClusterQualificationRequest is what a qualification of one cluster must
// exercise, and where its record goes: the --print output of the runner.
type ClusterQualificationRequest struct {
	Namespace      string                  `json:"namespace"`
	Name           string                  `json:"name"`
	Key            ClusterQualificationKey `json:"key"`
	RequiredChecks []string                `json:"requiredChecks"`
}

// TenantProvisioningReady is the static per-allocation check: the current
// Tenant is Ready and every namespace exists with its UID and BaselineReady.
// Nothing is recorded per allocation.
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

// CurrentClusterQualificationRequest performs fresh reads of one NaaS cluster
// and emits the key a record must carry. It issues no attestation, sets no
// passing checks and writes nothing.
func CurrentClusterQualificationRequest(ctx context.Context, management client.Reader, workload client.Client, namespace, cluster string) (*ClusterQualificationRequest, error) {
	parent := &cloud.Cluster{}
	if err := management.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cluster}, parent); err != nil {
		return nil, err
	}
	if parent.UID == "" || !parent.DeletionTimestamp.IsZero() || parent.Spec.NaaS == nil || !*parent.Spec.NaaS {
		return nil, fmt.Errorf("cluster must be a current NaaS cluster")
	}
	bootstrap, err := VerifyTenantBootstrap(ctx, workload)
	if err != nil {
		return nil, err
	}
	spec := convert.ClusterSpecFromAPI(parent.Spec, parent.Labels[cloudstore.LabelNetworkID], nil)
	key, err := ClusterQualificationKeyFor(parent, bootstrap.Revision(), spec)
	if err != nil {
		return nil, err
	}
	return &ClusterQualificationRequest{Namespace: QualificationNamespace, Name: ClusterQualificationName, Key: key, RequiredChecks: RequiredQualificationChecks()}, nil
}
