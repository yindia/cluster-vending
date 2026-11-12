package kubestore

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/apis/platform/crds"
	platformv1alpha1 "github.com/yindia/cluster-vending/pkg/apis/platform/v1alpha1"
)

// fieldOwner is the server-side apply field manager of the definitions.
const fieldOwner = "cluster-vending"

// Install prepares the management cluster for the store: it applies the
// CustomResourceDefinitions of the platform's kinds (server-side apply,
// so a newer build updates them) and creates the platform namespace when
// it is missing. The kinds are served a moment later; wait for Kinds
// before the first read.
func Install(ctx context.Context, c client.Client, namespace string) error {
	definitions, err := crds.All()
	if err != nil {
		return fmt.Errorf("read the platform's CustomResourceDefinitions: %w", err)
	}
	for _, crd := range definitions {
		delete(crd.Object, "status")
		err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(crd), client.FieldOwner(fieldOwner), client.ForceOwnership)
		if err != nil {
			return fmt.Errorf("apply CustomResourceDefinition %s: %w", crd.GetName(), err)
		}
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", namespace, err)
	}
	return nil
}

// Kinds are the kinds the store reads and writes.
func Kinds() []schema.GroupVersionKind {
	kinds := []string{"Tenant", "CloudAccount", "Network", "Cluster", "NodePool", "ManagedResource", "Claim"}
	out := make([]schema.GroupVersionKind, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, platformv1alpha1.GroupVersion.WithKind(kind))
	}
	return out
}
