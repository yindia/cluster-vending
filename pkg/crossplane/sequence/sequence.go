// Package sequence makes the management cluster match a set of Crossplane
// objects, in order. A Set is the exact objects an owner (a network, a
// cluster) should have, each with what it waits for; Apply creates what
// can be created now, holds back what waits, and removes what the set no
// longer has. It is safe to repeat: callers apply the same set again until
// the result is complete.
//
// The package knows nothing about networks or clusters, and nothing about
// where an owner's objects are remembered: that is the Inventory the
// caller passes in.
package sequence

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// Condition is something an item waits for before it is created.
type Condition struct {
	object client.Object
	state  cloudv1.ManagedResourceState
	text   string
}

// Ready is met once obj (an object of the same set, named by its
// namespace and name) is Ready in the management cluster.
func Ready(obj client.Object) Condition {
	return Condition{object: obj, state: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, text: "ready"}
}

// Item is one object of a set.
type Item struct {
	// Object is the object as it is applied: a typed object of the
	// provider's or Crossplane's own API packages.
	Object client.Object
	// After holds the item back until every condition is met. It gates the
	// item's creation only: an object the owner already has is applied
	// whatever its conditions say, so a dependency that is briefly not
	// Ready during an update never takes its dependents away.
	After []Condition
}

// Set is the objects an owner should have, in the order they are applied.
type Set []Item

// Of is a set of objects that wait for nothing.
func Of(objects ...client.Object) Set {
	set := make(Set, 0, len(objects))
	for _, obj := range objects {
		set = append(set, Item{Object: obj})
	}
	return set
}

// Objects are the set's objects, in order.
func (s Set) Objects() []client.Object {
	out := make([]client.Object, 0, len(s))
	for _, item := range s {
		out = append(out, item.Object)
	}
	return out
}

// Entry is one object an owner has, as its inventory remembers it.
type Entry struct {
	managed.Ref
	// Kept marks an object that no set describes and that stays when it is
	// missing from one (carried over from an import).
	Kept bool
}

// Inventory remembers which objects an owner has, also those a later set
// leaves out: without it they could not be found to be removed.
type Inventory interface {
	// List returns the owner's objects, oldest first.
	List(ctx context.Context) ([]Entry, error)
	// Add remembers objects; one already remembered is left as it is.
	Add(ctx context.Context, objects []client.Object) error
	// Remove forgets one object.
	Remove(ctx context.Context, entry Entry) error
}

// Waiting is an item that was held back, and what for.
type Waiting struct {
	Ref managed.Ref
	For []string
}

// Result is what one Apply did.
type Result struct {
	// Applied is how many objects were applied.
	Applied int
	// Waiting are the items held back. While there are any, nothing was
	// removed: an object held back is not a dropped one.
	Waiting []Waiting
	// Removed are the objects the set no longer has, now deleted.
	Removed []managed.Ref
}

// Complete reports whether the whole set was applied: nothing waits.
func (r Result) Complete() bool { return len(r.Waiting) == 0 }

// Engine applies sets.
type Engine struct {
	Objects managed.Client
}

func (e Engine) scheme() *runtime.Scheme { return e.Objects.Kube.Scheme() }

// Apply makes the owner's objects match set. In order: every item that
// waits for nothing (or that the owner already has) is remembered, then
// applied; the others are reported as waiting. Once nothing waits, what
// the inventory has and the set does not is deleted and forgotten, but
// for entries marked Kept. An error stops it; applying again continues.
func (e Engine) Apply(ctx context.Context, set Set, inv Inventory) (Result, error) {
	entries, err := inv.List(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list the owner's objects: %w", err)
	}
	has := make(map[string]bool, len(entries))
	for _, entry := range entries {
		has[key(entry.Ref)] = true
	}
	var res Result
	var ready []client.Object
	wanted := make(map[string]bool, len(set))
	for _, item := range set {
		ref, err := managed.RefOf(item.Object, e.scheme())
		if err != nil {
			return Result{}, err
		}
		wanted[key(ref)] = true
		if unmet, err := e.unmet(ctx, item, has[key(ref)]); err != nil {
			return Result{}, err
		} else if len(unmet) > 0 {
			res.Waiting = append(res.Waiting, Waiting{Ref: ref, For: unmet})
			continue
		}
		ready = append(ready, item.Object)
	}
	// Remembered first: an object is never in the cluster without the
	// owner knowing of it, so a failed apply can always be undone.
	if err := inv.Add(ctx, ready); err != nil {
		return Result{}, fmt.Errorf("remember the owner's objects: %w", err)
	}
	for _, obj := range ready {
		if err := kube.Apply(ctx, e.Objects.Kube, obj); err != nil {
			return res, err
		}
		res.Applied++
	}
	if !res.Complete() {
		return res, nil
	}
	for _, entry := range entries {
		if entry.Kept || wanted[key(entry.Ref)] {
			continue
		}
		if err := e.Objects.DeleteDropped(ctx, entry.Ref); err != nil {
			return res, err
		}
		if err := inv.Remove(ctx, entry); err != nil {
			return res, fmt.Errorf("forget %s %s: %w", entry.Kind, entry.Name, err)
		}
		res.Removed = append(res.Removed, entry.Ref)
	}
	return res, nil
}

// unmet are the conditions item still waits for; none when the owner
// already has the object.
func (e Engine) unmet(ctx context.Context, item Item, exists bool) ([]string, error) {
	if exists {
		return nil, nil
	}
	var out []string
	for _, cond := range item.After {
		ref, err := managed.RefOf(cond.object, e.scheme())
		if err != nil {
			return nil, err
		}
		views, err := e.Objects.Observe(ctx, []managed.Ref{ref})
		if err != nil {
			return nil, err
		}
		if views[0].GetState() != cond.state {
			out = append(out, fmt.Sprintf("%s %s to be %s", ref.Kind, ref.Name, cond.text))
		}
	}
	return out, nil
}

func key(r managed.Ref) string { return r.APIVersion + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name }
