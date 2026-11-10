package kube

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// FieldOwner is the server-side apply field manager of everything the
// server writes, so `kubectl get --show-managed-fields` tells the platform's
// fields from anyone else's.
const FieldOwner = "cluster-vending"

// Apply creates or updates obj (a typed object of the client's scheme, e.g.
// a Crossplane Provider or a provider-aws ProviderConfig) with server-side
// apply as FieldOwner, taking over conflicting fields. Only the fields obj
// sets are owned; status is never written. The API server validates obj
// against the CRD schema; a rejected object returns an error for which
// apierrors.IsInvalid is true.
func Apply(ctx context.Context, c client.Client, obj client.Object, opts ...client.ApplyOption) error {
	u, err := applyConfiguration(obj, c.Scheme())
	if err != nil {
		return err
	}
	opts = append([]client.ApplyOption{client.FieldOwner(FieldOwner), client.ForceOwnership}, opts...)
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), opts...); err != nil {
		return fmt.Errorf("apply %s %s: %w", u.GetKind(), client.ObjectKeyFromObject(obj), err)
	}
	return nil
}

// applyConfiguration turns a typed object into the apply request body:
// apiVersion and kind from the scheme, without status and the zero
// creationTimestamp the Go types always serialize.
func applyConfiguration(obj client.Object, scheme *runtime.Scheme) (*unstructured.Unstructured, error) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return nil, fmt.Errorf("apply %T: %w", obj, err)
	}
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("apply %T: %w", obj, err)
	}
	u := &unstructured.Unstructured{Object: content}
	u.SetGroupVersionKind(gvk)
	delete(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	return u, nil
}

// ApplyFields server-side applies u (a partial object: apiVersion, kind,
// name, namespace and only the fields to own) as owner, without forcing.
// The fields stay owner's: another manager's later apply that does not set
// them leaves them alone, and applying u again without a field removes it.
// A field another manager owns with a different value is a conflict error
// (apierrors.IsConflict).
func ApplyFields(ctx context.Context, c client.Client, u *unstructured.Unstructured, owner string) error {
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(owner)); err != nil {
		return fmt.Errorf("apply %s %s/%s as %s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), owner, err)
	}
	return nil
}
