package cloudstore

import (
	"context"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/config/crd"
	"github.com/yindia/cluster-vending/pkg/buildserial"
)

// fieldOwner is the server-side apply field manager of the definitions.
const fieldOwner = "cluster-vending"

// Install prepares the management cluster for the store: it applies the
// CustomResourceDefinitions of the cloud.cluster-vending.io kinds
// (server-side apply, so a newer build updates them), each stamped with
// the build's serial (buildserial). A definition a newer build installed
// is not applied: its older schema would prune the newer fields of every
// stored object. That is logged as an error and fails the ready check
// (buildserial.Check); the definition stays as the newer build left it.
// The kinds are served a moment later; wait for Kinds before the first
// read. Tenant namespaces are created with their tenants.
func Install(ctx context.Context, c client.Client) error {
	definitions, err := crd.All()
	if err != nil {
		return fmt.Errorf("read the CustomResourceDefinitions: %w", err)
	}
	for _, definition := range definitions {
		newer, serial, err := installedByNewer(ctx, c, definition.GetName())
		if err != nil {
			return err
		}
		if newer {
			buildserial.Refuse("CustomResourceDefinition "+definition.GetName(), serial)
			slog.ErrorContext(ctx, "NOT applying a CustomResourceDefinition a newer build installed: this build is older; run the newer build",
				"crd", definition.GetName(), "installed_serial", serial, "build_serial", buildserial.Serial)
			continue
		}
		err = c.Apply(ctx, client.ApplyConfigurationFromUnstructured(stampedCRD(definition)), client.FieldOwner(fieldOwner), client.ForceOwnership)
		if err != nil {
			return fmt.Errorf("apply CustomResourceDefinition %s: %w", definition.GetName(), err)
		}
	}
	return nil
}

// crdGVK is the kind of a CustomResourceDefinition.
var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// installedByNewer reports whether the CustomResourceDefinition name was
// installed by a newer build, and its serial; false when it is missing.
func installedByNewer(ctx context.Context, c client.Client, name string) (bool, int64, error) {
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(crdGVK)
	err := c.Get(ctx, client.ObjectKey{Name: name}, current)
	if apierrors.IsNotFound(err) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("read CustomResourceDefinition %s: %w", name, err)
	}
	newer, serial := buildserial.Newer(current.GetAnnotations())
	return newer, serial, nil
}

// stampedCRD is definition as applied: without status, with the build's
// serial. definition is not changed.
func stampedCRD(definition *unstructured.Unstructured) *unstructured.Unstructured {
	out := definition.DeepCopy()
	delete(out.Object, "status")
	out.SetAnnotations(buildserial.Stamp(out.GetAnnotations()))
	return out
}

// Kinds are the platform's kinds: those the store reads and writes, and
// the operator's kinds (Release, ReleaseChannel, ReleaseHold and the
// component catalog, ComponentDefinition) the controller reconciles and
// the server reads. All are installed together
// (Install) and served once their definitions are.
func Kinds() []schema.GroupVersionKind {
	kinds := []string{"Tenant", "CloudAccount", "Network", "Cluster", "Release", "ReleaseChannel", "ReleaseHold",
		"Component", "ComponentDefinition"}
	out := make([]schema.GroupVersionKind, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, cloudv1alpha1.GroupVersion.WithKind(kind))
	}
	return out
}
