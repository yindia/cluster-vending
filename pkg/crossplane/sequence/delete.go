package sequence

import (
	"context"
	"fmt"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// DeleteOrder is the order an owner's objects are deleted in
// (managed.DeleteOrder): tiers of kinds, each deleted once the earlier
// ones are gone.
type DeleteOrder = managed.DeleteOrder

// Deletion is how far the deletion of an owner's objects is.
type Deletion struct {
	// Entries are the owner's objects as its inventory has them; Views
	// their state before this step, in the same order.
	Entries []Entry
	Views   []*cloudv1.ManagedResource
	// Left is how many objects were still in the cluster.
	Left int
}

// Done reports whether every object is gone.
func (d Deletion) Done() bool { return d.Left == 0 }

// Delete moves the deletion of an owner's objects one step on, in order:
// the objects of the first tier that still has any are deleted, the next
// tier once that one is gone (DeleteOrder), so the cloud deletes
// dependents before what they depend on. Usages whose user is gone are
// released on the way. It is safe to repeat: callers call it until the
// result is Done, then forget the inventory.
func (e Engine) Delete(ctx context.Context, inv Inventory, order DeleteOrder) (Deletion, error) {
	entries, err := inv.List(ctx)
	if err != nil {
		return Deletion{}, fmt.Errorf("list the owner's objects: %w", err)
	}
	refs := make([]managed.Ref, 0, len(entries))
	for _, entry := range entries {
		refs = append(refs, entry.Ref)
	}
	views, err := e.Objects.Observe(ctx, refs)
	if err != nil {
		return Deletion{}, err
	}
	out := Deletion{Entries: entries, Views: views}
	if err := e.Objects.ReleaseOrphanUsages(ctx, refs, views); err != nil {
		return out, err
	}
	out.Left, err = e.Objects.DeleteNext(ctx, refs, views, order)
	return out, err
}
