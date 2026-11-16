package naas

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

type AccessQualificationRequest struct {
	Namespace      string               `json:"namespace"`
	Name           string               `json:"name"`
	Binding        QualificationBinding `json:"binding"`
	RequiredChecks []string             `json:"requiredChecks"`
}

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

// CurrentAccessQualificationRequest performs fresh reads and emits only a test
// request. It issues no attestation, sets no passing checks and writes nothing.
func CurrentAccessQualificationRequest(ctx context.Context, management client.Reader, workload client.Client, namespace, allocation string) (*AccessQualificationRequest, error) {
	sub := &cloud.SubTenant{}
	if err := management.Get(ctx, client.ObjectKey{Namespace: namespace, Name: allocation}, sub); err != nil {
		return nil, err
	}
	parent := &cloud.Cluster{}
	if err := management.Get(ctx, client.ObjectKey{Namespace: namespace, Name: sub.Spec.ClusterRef.Name}, parent); err != nil {
		return nil, err
	}
	registration := &cloud.Tenant{}
	if err := management.Get(ctx, client.ObjectKey{Name: sub.Spec.TenantRef.Name}, registration); err != nil {
		return nil, err
	}
	if sub.UID == "" || !sub.DeletionTimestamp.IsZero() || string(parent.UID) != sub.Spec.ClusterRef.UID ||
		string(registration.UID) != sub.Spec.TenantRef.UID || parent.Labels[cloudstore.LabelID] != sub.Spec.ClusterRef.ID || parent.Labels[cloudstore.LabelTenant] != sub.Spec.TenantRef.Name {
		return nil, fmt.Errorf("allocation and parent registration identities must be current")
	}
	spec := convert.ClusterSpecFromAPI(parent.Spec, parent.Labels[cloudstore.LabelNetworkID], nil)
	access, err := ResolveAccess(sub.Spec.Access, spec)
	if err != nil || len(access.Bindings) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("group access must be configured before requesting qualification")
	}
	associationUID, err := ObserveAccessOIDC(ctx, management, parent, spec)
	if err != nil {
		return nil, err
	}
	bootstrap, err := VerifyTenantBootstrap(ctx, workload)
	if err != nil {
		return nil, err
	}
	desired, err := RenderTenantWithAccess(sub, parent.Status.CurrentKubernetesVersion, bootstrap.Revision(), spec)
	if err != nil {
		return nil, err
	}
	remote := &unstructured.Unstructured{}
	remote.SetGroupVersionKind(TenantGVK)
	if err := workload.Get(ctx, client.ObjectKey{Name: desired.GetName()}, remote); err != nil {
		return nil, err
	}
	if !OwnTenant(remote, sub) || !EqualTenantSpec(remote, desired) || remote.GetAnnotations()[SpecAnnotation] != desired.GetAnnotations()[SpecAnnotation] {
		return nil, fmt.Errorf("current remote Tenant does not match desired security configuration")
	}
	inventory, _, err := ObserveTenantNamespaces(ctx, workload, remote, sub, bootstrap)
	if err != nil {
		return nil, err
	}
	if !TenantProvisioningReady(sub, remote, inventory) {
		return nil, fmt.Errorf("current Tenant Ready and all namespace baselines must be observed before qualification")
	}
	if err := ObserveAccessBindings(ctx, workload, sub, remote, access); err != nil && !errors.Is(err, ErrAccessBindingsPending) {
		return nil, fmt.Errorf("remove unexpected or temporary test bindings before qualification: %w", err)
	}
	binding, err := BindAccessQualification(sub, parent, remote, inventory, bootstrap.Revision(), access, associationUID)
	if err != nil {
		return nil, err
	}
	return &AccessQualificationRequest{Namespace: QualificationNamespace, Name: QualificationName(string(sub.UID)), Binding: binding, RequiredChecks: RequiredQualificationChecks()}, nil
}
