// Package catalog installs the component catalog this build ships
// (definitions/) into the management cluster, and keeps the definitions it
// shipped up to date: Sync.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// ShippedHashAnnotation holds the SpecHash of the spec a build shipped: a
// definition whose spec still has that hash was not edited since.
const ShippedHashAnnotation = "cloud.cluster-vending.io/shipped-hash"

// Result names what Sync did with each definition.
type Result struct {
	// Created: missing, created from this build.
	Created []string
	// Updated: shipped by an earlier build and not edited since; replaced
	// with this build's spec.
	Updated []string
	// Existing: already this build's spec.
	Existing []string
	// Adopted: not labelled as shipped, but equal to this build's spec;
	// labelled now, so later builds update it.
	Adopted []string
	// Edited: shipped, then changed by the platform team; theirs, left.
	Edited []string
	// Foreign: not labelled as shipped and not this build's spec; left
	// (kubectl replace -f the file, or delete it and restart, to take the
	// build's).
	Foreign []string
	// Deleted: shipped, not edited, and no longer in the build.
	Deleted []string
	// Kept: no longer in the build and not edited, but in use; deleted
	// once nothing uses it (the definition's in-use guard holds it).
	Kept []string
}

// Install syncs the shipped definitions. The CustomResourceDefinition of
// ComponentDefinition must be served.
func Install(ctx context.Context, c client.Client) (Result, error) {
	defs, err := Definitions()
	if err != nil {
		return Result{}, fmt.Errorf("read the shipped catalog: %w", err)
	}
	return Sync(ctx, c, defs)
}

// SpecHash is the hash of a definition spec as shipped: the SHA-256 of its
// JSON (a Go struct marshals in field order, so the same spec always gives
// the same hash). The disabled flag is the platform team's and is left out:
// switching a definition off is not an edit that stops later builds from
// updating the rest of it.
func SpecHash(spec cloudv1alpha1.ComponentDefinitionSpec) string {
	spec.Disabled = false
	raw, _ := json.Marshal(spec) // these types always marshal
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Sync brings the management cluster's definitions to this build's (defs):
//
//   - missing: created, labelled shipped, with the hash of its spec;
//   - shipped and unedited (its spec still has the recorded hash): replaced
//     with the build's spec when that differs, keeping the platform team's
//     disabled flag;
//   - shipped and edited: left, the platform team's;
//   - not labelled shipped: adopted when equal to the build's spec,
//     otherwise left;
//   - shipped, unedited and no longer in the build: deleted (the in-use
//     guard holds it while something uses it);
//   - anything else (the platform team's own definitions): left.
//
// The inputs are not changed.
func Sync(ctx context.Context, c client.Client, defs []*cloudv1alpha1.ComponentDefinition) (Result, error) {
	var out Result
	inBuild := make(map[string]bool, len(defs))
	for _, def := range defs {
		inBuild[def.Name] = true
		if err := syncOne(ctx, c, def, &out); err != nil {
			return out, err
		}
	}
	list := &cloudv1alpha1.ComponentDefinitionList{}
	if err := c.List(ctx, list); err != nil {
		return out, fmt.Errorf("list component definitions: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	for i := range list.Items {
		d := &list.Items[i]
		if inBuild[d.Name] || !isShipped(d) {
			continue
		}
		if !unedited(d) {
			out.Edited = append(out.Edited, d.Name)
			continue
		}
		if !d.DeletionTimestamp.IsZero() {
			out.Kept = append(out.Kept, d.Name)
			continue
		}
		if err := c.Delete(ctx, d); err != nil && !apierrors.IsNotFound(err) {
			return out, fmt.Errorf("delete component definition %s: %w", d.Name, err)
		}
		out.Deleted = append(out.Deleted, d.Name)
	}
	return out, nil
}

// syncOne applies Sync's rules to one shipped definition.
func syncOne(ctx context.Context, c client.Client, def *cloudv1alpha1.ComponentDefinition, out *Result) error {
	current := &cloudv1alpha1.ComponentDefinition{}
	err := c.Get(ctx, client.ObjectKey{Name: def.Name}, current)
	if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, asShipped(def.DeepCopy())); err != nil {
			return fmt.Errorf("create component definition %s: %w", def.Name, err)
		}
		out.Created = append(out.Created, def.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read component definition %s: %w", def.Name, err)
	}
	want := SpecHash(def.Spec)
	switch {
	case !isShipped(current) && SpecHash(current.Spec) == want:
		adopted := asShipped(current.DeepCopy())
		if err := c.Update(ctx, adopted); err != nil {
			return fmt.Errorf("adopt component definition %s: %w", def.Name, err)
		}
		out.Adopted = append(out.Adopted, def.Name)
	case !isShipped(current):
		out.Foreign = append(out.Foreign, def.Name)
	case !unedited(current):
		out.Edited = append(out.Edited, def.Name)
	case SpecHash(current.Spec) == want:
		out.Existing = append(out.Existing, def.Name)
	default:
		updated := current.DeepCopy()
		disabled := updated.Spec.Disabled
		updated.Spec = *def.Spec.DeepCopy() // whole: fields the new build dropped go
		updated.Spec.Disabled = disabled    // the platform team's
		if err := c.Update(ctx, asShipped(updated)); err != nil {
			return fmt.Errorf("update component definition %s: %w", def.Name, err)
		}
		out.Updated = append(out.Updated, def.Name)
	}
	return nil
}

// asShipped labels d as shipped and records the hash of its spec.
func asShipped(d *cloudv1alpha1.ComponentDefinition) *cloudv1alpha1.ComponentDefinition {
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Labels[cloudv1alpha1.LabelShipped] = "true"
	d.Annotations[ShippedHashAnnotation] = SpecHash(d.Spec)
	return d
}

// isShipped: a build created or adopted d.
func isShipped(d *cloudv1alpha1.ComponentDefinition) bool {
	return d.Labels[cloudv1alpha1.LabelShipped] == "true" && d.Annotations[ShippedHashAnnotation] != ""
}

// unedited: d's spec is still what the build that shipped it recorded.
func unedited(d *cloudv1alpha1.ComponentDefinition) bool {
	return d.Annotations[ShippedHashAnnotation] == SpecHash(d.Spec)
}
