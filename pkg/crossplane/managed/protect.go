package managed

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RetainAnnotation marks an imported object whose cloud resource others
// use too (a log group or role shared with other VPCs): it is managed
// without Delete, so deleting its owner leaves it in the cloud, always.
const RetainAnnotation = "cluster-vending.io/retain"

// The platform renders its stateful objects (VPCs, subnets, gateways,
// route tables, EKS clusters and node groups, keys, roles, policies, the
// OIDC provider, log groups) managed without Delete while their owner
// exists: an object deleted by anything but the owner's own deletion (by
// hand, or dropped by a new build's render) leaves its cloud resource be.
// The owner's deletion, and a render the owner's update drops, switch an
// object to full management first (allowDelete), so the cloud resource
// goes with it.

type keepDroppedKey struct{}

// KeepDropped marks ctx as an apply whose dropped objects keep their cloud
// resources (a code change: what a new build no longer renders may be its
// mistake). Without the mark a dropped object is deleted in the cloud.
func KeepDropped(ctx context.Context) context.Context {
	return context.WithValue(ctx, keepDroppedKey{}, true)
}

func keepsDropped(ctx context.Context) bool {
	keep, _ := ctx.Value(keepDroppedKey{}).(bool)
	return keep
}

// protects reports whether management policies are the platform's
// "everything but Delete" (PolicyNoDelete): ones it switches for a delete.
// Observe-only (imported) objects are never switched.
func protects(policies []string) bool {
	return slices.Contains(policies, "Create") && !slices.Contains(policies, "Delete") && !slices.Contains(policies, "*")
}

// allowDelete switches the object of r to full management when the
// platform protects it from deletion (protects) and it is not shared
// (RetainAnnotation), so deleting it deletes its cloud resource. An
// object that is gone, or of a kind not installed, needs nothing.
func (c Client) allowDelete(ctx context.Context, r Ref) error {
	u := r.empty()
	err := c.Kube.Get(ctx, r.key(), u)
	switch {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return nil
	case err != nil:
		return fmt.Errorf("read %s %s: %w", r.Kind, r.Name, err)
	}
	if u.GetAnnotations()[RetainAnnotation] == "true" || !u.GetDeletionTimestamp().IsZero() {
		return nil
	}
	policies, found, _ := unstructured.NestedStringSlice(u.Object, "spec", "managementPolicies")
	if !found || !protects(policies) {
		return nil // unreadable or not protected: deleted as it is
	}
	before := u.DeepCopy()
	if err := unstructured.SetNestedStringSlice(u.Object, []string{"*"}, "spec", "managementPolicies"); err != nil {
		return err
	}
	if err := c.Kube.Patch(ctx, u, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("allow the deletion of %s %s: %w", r.Kind, r.Name, err)
	}
	return nil
}
