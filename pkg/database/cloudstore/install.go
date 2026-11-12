package cloudstore

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/controller/api/v1alpha1"
	"github.com/yindia/cluster-vending/controller/config/crd"
)

// fieldOwner is the server-side apply field manager of the definitions.
const fieldOwner = "cluster-vending"

// Install prepares the management cluster for the store: it applies the
// CustomResourceDefinitions of the cloud.cluster-vending.io kinds
// (server-side apply, so a newer build updates them). The kinds are
// served a moment later; wait for Kinds before the first read. Tenant
// namespaces are created with their tenants.
func Install(ctx context.Context, c client.Client) error {
	definitions, err := crd.All()
	if err != nil {
		return fmt.Errorf("read the CustomResourceDefinitions: %w", err)
	}
	for _, definition := range definitions {
		delete(definition.Object, "status")
		err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(definition), client.FieldOwner(fieldOwner), client.ForceOwnership)
		if err != nil {
			return fmt.Errorf("apply CustomResourceDefinition %s: %w", definition.GetName(), err)
		}
	}
	return nil
}

// Kinds are the kinds the store reads and writes.
func Kinds() []schema.GroupVersionKind {
	kinds := []string{"CloudAccount", "Network", "Cluster"}
	out := make([]schema.GroupVersionKind, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, cloudv1alpha1.GroupVersion.WithKind(kind))
	}
	return out
}
