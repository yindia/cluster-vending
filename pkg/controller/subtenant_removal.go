package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// Sub-tenants (PROP-05) and the Capsule component were removed from the
// platform. This file is the one-time migration of a management cluster a
// build with them ran on. Both steps are idempotent: on a cluster that
// never had them, or was migrated already, they read and change nothing.

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=subtenants,verbs=get;list;patch

// subTenantGVK is the removed SubTenant kind, read as unstructured: this
// build has no Go type of it.
var subTenantGVK = cloudv1alpha1.GroupVersion.WithKind("SubTenant")

// subTenantCRDName is the removed kind's CustomResourceDefinition.
const subTenantCRDName = "subtenants.cloud.cluster-vending.io"

// crdGVK is the kind of a CustomResourceDefinition.
var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// removedTenancyRole is the ComponentDefinition role Capsule played
// (NamespaceTenancy), removed with sub-tenants; the CRD refuses it now.
const removedTenancyRole = "NamespaceTenancy"

// subTenantCRDGone bounds the wait for Kubernetes to delete the SubTenant
// CRD (it deletes the CRD's remaining objects first), and how often it
// is checked meanwhile.
var (
	subTenantCRDGone     = 2 * time.Minute
	subTenantCRDInterval = 2 * time.Second
)

// ReleaseRemovedSubTenants releases the SubTenant objects a build with
// sub-tenants left, and deletes their CRD. Each object's finalizers are
// removed WITHOUT the old controller's cleanup in the workload cluster:
// the tenant keeps the namespaces and RBAC it has there. Then the CRD is
// deleted and Kubernetes deletes the objects that are left. It returns the
// released objects ("<namespace>/<name>"), each also logged. Without the
// CRD it does nothing.
//
// Run it only as the leader (SubTenantRemoval): an older controller that
// still leads would put its finalizer back and run its cleanup once the
// CRD's deletion deletes the objects.
func ReleaseRemovedSubTenants(ctx context.Context, c client.Client, logger *slog.Logger) ([]string, error) {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: subTenantCRDName}, crd); apierrors.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("read CustomResourceDefinition %s: %w", subTenantCRDName, err)
	}
	released, err := releaseSubTenants(ctx, c, logger)
	if err != nil {
		return released, err
	}
	if err := client.IgnoreNotFound(c.Delete(ctx, crd)); err != nil {
		return released, fmt.Errorf("delete CustomResourceDefinition %s: %w", subTenantCRDName, err)
	}
	logger.InfoContext(ctx, "sub-tenants were removed: deleted their CustomResourceDefinition", "crd", subTenantCRDName, "released", len(released))
	// The CRD goes once its objects are gone; release any that got a
	// finalizer back meanwhile, so the deletion cannot hang.
	err = wait.PollUntilContextTimeout(ctx, subTenantCRDInterval, subTenantCRDGone, true, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, client.ObjectKey{Name: subTenantCRDName}, crd)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("read CustomResourceDefinition %s: %w", subTenantCRDName, err)
		}
		more, err := releaseSubTenants(ctx, c, logger)
		released = append(released, more...)
		return false, err
	})
	if err != nil {
		return released, fmt.Errorf("wait for CustomResourceDefinition %s to be deleted: %w", subTenantCRDName, err)
	}
	return released, nil
}

// releaseSubTenants removes every finalizer of every SubTenant object.
func releaseSubTenants(ctx context.Context, c client.Client, logger *slog.Logger) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(subTenantGVK.GroupVersion().WithKind(subTenantGVK.Kind + "List"))
	if err := c.List(ctx, list); meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return nil, nil // no longer served: nothing to release
	} else if err != nil {
		return nil, fmt.Errorf("list SubTenants: %w", err)
	}
	var released []string
	var errs []error
	for i := range list.Items {
		obj := &list.Items[i]
		finalizers := obj.GetFinalizers()
		if len(finalizers) == 0 {
			continue
		}
		key := obj.GetNamespace() + "/" + obj.GetName()
		// A merge patch of the finalizers only: the object's spec is not
		// validated again, and a concurrent change is not overwritten.
		patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))
		if err := client.IgnoreNotFound(c.Patch(ctx, obj, patch)); err != nil {
			errs = append(errs, fmt.Errorf("release SubTenant %s: %w", key, err))
			continue
		}
		released = append(released, key)
		logger.InfoContext(ctx, "sub-tenants were removed: released a SubTenant without cleaning up its workload cluster; the tenant keeps its namespaces and RBAC there",
			"subtenant", key, "finalizers", finalizers)
	}
	slices.Sort(released)
	return released, errors.Join(errs...)
}

// RetireNamespaceTenancy switches off what is left of Capsule in the
// catalog: every ComponentDefinition that still declares the removed
// NamespaceTenancy role. Run it after catalog.Sync, which deletes the
// shipped definition (its finalizer holds it while clusters have Capsule).
//
//   - Each one loses the role: the CRD refuses it on an update, so the
//     in-use finalizer of the one being deleted could never be removed.
//   - One that is not being deleted (the platform team edited it, so
//     catalog.Sync kept it) is also disabled: without the role it would be
//     an ordinary platform component and install on every cluster.
//
// Every cluster that has Capsule then removes it through its gated
// catalog change (Helm uninstall, whose post-delete hook removes Capsule's
// webhook configurations, and its IAM). It returns the definitions it
// changed, each also logged.
func RetireNamespaceTenancy(ctx context.Context, c client.Client, logger *slog.Logger) ([]string, error) {
	defs := &cloudv1alpha1.ComponentDefinitionList{}
	if err := c.List(ctx, defs); err != nil {
		return nil, fmt.Errorf("list ComponentDefinitions: %w", err)
	}
	var retired []string
	var errs []error
	for i := range defs.Items {
		def := &defs.Items[i]
		if string(def.Spec.Role) != removedTenancyRole {
			continue
		}
		deleting := !def.DeletionTimestamp.IsZero()
		patch := `{"spec":{"role":null}}`
		if !deleting {
			patch = `{"spec":{"role":null,"disabled":true}}`
		}
		if err := client.IgnoreNotFound(c.Patch(ctx, def, client.RawPatch(types.MergePatchType, []byte(patch)))); err != nil {
			errs = append(errs, fmt.Errorf("retire ComponentDefinition %s: %w", def.Name, err))
			continue
		}
		retired = append(retired, def.Name)
		logger.InfoContext(ctx, "Capsule was removed: cleared the NamespaceTenancy role of a component definition; clusters uninstall it as a catalog change",
			"definition", def.Name, "disabled", !deleting, "deleting", deleting)
	}
	slices.Sort(retired)
	return retired, errors.Join(errs...)
}

// SubTenantRemoval runs ReleaseRemovedSubTenants once this replica leads
// (a manager runnable needs leader election unless it says otherwise): by
// then no older controller reconciles SubTenants. A failure is logged and
// does not stop the manager; the next start tries again.
func SubTenantRemoval(c client.Client, logger *slog.Logger) manager.Runnable {
	return manager.RunnableFunc(func(ctx context.Context) error {
		if _, err := ReleaseRemovedSubTenants(ctx, c, logger); err != nil {
			logger.ErrorContext(ctx, "sub-tenants were removed: releasing the SubTenants left on the management cluster failed; the next start tries again",
				"crd", subTenantCRDName, "error", err)
		}
		return nil
	})
}
