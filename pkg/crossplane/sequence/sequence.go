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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// Condition is something an item waits for before it is created. There
// are two: Ready and Field.
type Condition struct {
	object client.Object
	// path and value are set for Field; Ready has none.
	path  []string
	value string
}

// Ready is met once obj (named by its namespace and name) is Ready in the
// management cluster.
func Ready(obj client.Object) Condition { return Condition{object: obj} }

// Field is met once the string field of obj at path (in the object as the
// management cluster has it, e.g. "status", "atProvider", "status") is
// value. For what Ready does not say: an EKS cluster is Ready while it is
// still being created or updated, and ACTIVE only once it can be used.
func Field(obj client.Object, value string, path ...string) Condition {
	return Condition{object: obj, path: path, value: value}
}

// Item is one object of a set.
type Item struct {
	// Object is the object as it is applied: a typed object of the
	// provider's or Crossplane's own API packages.
	Object client.Object
	// After holds the item back until every condition is met. A condition
	// gates creation only, and only until it was passed once: an object
	// the owner already has is applied whatever its conditions say, and so
	// is a new object waiting for a condition that an object the owner has
	// waited for too. A dependency that is briefly not Ready during an
	// update neither takes its dependents away nor keeps new ones out.
	After []Condition
	// Build, when set, makes the object from what the management cluster
	// has now: for an object that needs a value which only exists once
	// another object does (a key's ARN, a cluster's issuer). It returns
	// the object to apply, with the namespace and name of Object, or nil
	// and what it waits for. Object is then only the item's identity and
	// is never applied itself. Unlike After, a Build that waits holds the
	// item back even when the owner already has the object: it cannot be
	// rendered at all.
	Build func(ctx context.Context, seen Observed) (obj client.Object, waitsFor string, err error)
	// Adjust, when set, changes the apply body (the object as
	// kube.ApplyObject makes it) before it is sent: for fields the
	// provider's Go type always serializes and that must not be applied.
	Adjust func(body *unstructured.Unstructured)
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

// Keeping is inv with the entries keep selects marked Kept: objects that
// stay although the set applied now does not have them (a node group
// still being retired; everything, while the set is only part of what
// the owner will have).
func Keeping(inv Inventory, keep func(Entry) bool) Inventory {
	return keeping{Inventory: inv, keep: keep}
}

type keeping struct {
	Inventory
	keep func(Entry) bool
}

func (k keeping) List(ctx context.Context) ([]Entry, error) {
	entries, err := k.Inventory.List(ctx)
	for i := range entries {
		entries[i].Kept = entries[i].Kept || k.keep(entries[i])
	}
	return entries, err
}

// Observed is what a Build may read: the objects as the management
// cluster has them, and which of them the owner has.
type Observed interface {
	// Get reads the object with obj's namespace and name into obj (a typed
	// object) and reports whether it exists.
	Get(ctx context.Context, obj client.Object) (bool, error)
	// Ready reports whether the object with obj's namespace and name is
	// Ready.
	Ready(ctx context.Context, obj client.Object) (bool, error)
	// Owned reports whether the owner's inventory has the object.
	Owned(obj client.Object) bool
}

// Observe is the Observed of an owner, as Apply gives it to the Builds of
// a set: for callers that need the same values without applying (a plan).
func (e Engine) Observe(ctx context.Context, inv Inventory) (Observed, error) {
	entries, err := inv.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the owner's objects: %w", err)
	}
	return e.observed(entries), nil
}

func (e Engine) observed(entries []Entry) observed {
	has := make(map[string]bool, len(entries))
	for _, entry := range entries {
		has[key(entry.Ref)] = true
	}
	return observed{engine: e, has: has}
}

type observed struct {
	engine Engine
	has    map[string]bool
}

func (o observed) Get(ctx context.Context, obj client.Object) (bool, error) {
	err := o.engine.Objects.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return false, nil
	}
	return err == nil, err
}

func (o observed) Ready(ctx context.Context, obj client.Object) (bool, error) {
	ref, err := managed.RefOf(obj, o.engine.scheme())
	if err != nil {
		return false, err
	}
	return o.engine.met(ctx, Ready(obj), ref)
}

func (o observed) Owned(obj client.Object) bool {
	ref, err := managed.RefOf(obj, o.engine.scheme())
	return err == nil && o.has[key(ref)]
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
	seen := e.observed(entries)
	has := seen.has
	var res Result
	var ready Set
	wanted := make(map[string]bool, len(set))
	checked := checks{engine: e, met: map[string]bool{}}
	if err := checked.passed(set, has); err != nil {
		return Result{}, err
	}
	for _, item := range set {
		ref, err := managed.RefOf(item.Object, e.scheme())
		if err != nil {
			return Result{}, err
		}
		wanted[key(ref)] = true
		if item.Build != nil {
			waitsFor, err := e.build(ctx, &item, ref, seen)
			if err != nil {
				return Result{}, err
			}
			if waitsFor != "" {
				res.Waiting = append(res.Waiting, Waiting{Ref: ref, For: []string{waitsFor}})
				continue
			}
		}
		if unmet, err := checked.unmet(ctx, item, has[key(ref)]); err != nil {
			return Result{}, err
		} else if len(unmet) > 0 {
			res.Waiting = append(res.Waiting, Waiting{Ref: ref, For: unmet})
			continue
		}
		ready = append(ready, item)
	}
	// Remembered first: an object is never in the cluster without the
	// owner knowing of it, so a failed apply can always be undone.
	if err := inv.Add(ctx, ready.Objects()); err != nil {
		return Result{}, fmt.Errorf("remember the owner's objects: %w", err)
	}
	for _, item := range ready {
		if err := e.applyItem(ctx, item); err != nil {
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

// build replaces item's object with what its Build makes, or says what
// the item waits for. The built object must be the one the item names.
func (e Engine) build(ctx context.Context, item *Item, ref managed.Ref, seen Observed) (string, error) {
	obj, waitsFor, err := item.Build(ctx, seen)
	if err != nil {
		return "", fmt.Errorf("build %s %s: %w", ref.Kind, ref.Name, err)
	}
	if obj == nil {
		if waitsFor == "" {
			waitsFor = "a value that does not exist yet"
		}
		return waitsFor, nil
	}
	built, err := managed.RefOf(obj, e.scheme())
	if err != nil {
		return "", err
	}
	if key(built) != key(ref) {
		return "", fmt.Errorf("build %s %s: it made %s %s", ref.Kind, ref.Name, built.Kind, built.Name)
	}
	item.Object = obj
	return "", nil
}

// applyItem server-side applies one item, through its Adjust when it has
// one.
func (e Engine) applyItem(ctx context.Context, item Item) error {
	if item.Adjust == nil {
		return kube.Apply(ctx, e.Objects.Kube, item.Object)
	}
	body, err := kube.ApplyObject(item.Object, e.scheme())
	if err != nil {
		return err
	}
	item.Adjust(body)
	return kube.Apply(ctx, e.Objects.Kube, body)
}

// checks evaluates the conditions of one Apply; each is read once,
// however many items wait for it.
type checks struct {
	engine Engine
	// met is whether a condition (by its text) holds, or was passed.
	met map[string]bool
}

// passed marks as met the conditions an object the owner has (has, by
// key) waited for: they were passed once.
func (c checks) passed(set Set, has map[string]bool) error {
	for _, item := range set {
		if len(item.After) == 0 {
			continue
		}
		ref, err := managed.RefOf(item.Object, c.engine.scheme())
		if err != nil {
			return err
		}
		if !has[key(ref)] {
			continue
		}
		for _, cond := range item.After {
			of, err := managed.RefOf(cond.object, c.engine.scheme())
			if err != nil {
				return err
			}
			c.met[cond.text(of)] = true
		}
	}
	return nil
}

// unmet are the conditions item still waits for; none when the owner
// already has the object.
func (c checks) unmet(ctx context.Context, item Item, exists bool) ([]string, error) {
	if exists {
		return nil, nil
	}
	var out []string
	for _, cond := range item.After {
		ref, err := managed.RefOf(cond.object, c.engine.scheme())
		if err != nil {
			return nil, err
		}
		text := cond.text(ref)
		met, known := c.met[text]
		if !known {
			if met, err = c.engine.met(ctx, cond, ref); err != nil {
				return nil, err
			}
			c.met[text] = met
		}
		if !met {
			out = append(out, text)
		}
	}
	return out, nil
}

// text says what the condition waits for; it also identifies it.
func (c Condition) text(ref managed.Ref) string {
	if len(c.path) == 0 {
		return fmt.Sprintf("%s %s to be ready", ref.Kind, ref.Name)
	}
	return fmt.Sprintf("%s %s to have %s %s", ref.Kind, ref.Name, strings.Join(c.path, "."), c.value)
}

// met reports whether cond holds for the object ref names as the
// management cluster has it. An object that does not exist meets nothing.
func (e Engine) met(ctx context.Context, cond Condition, ref managed.Ref) (bool, error) {
	if len(cond.path) == 0 {
		views, err := e.Objects.Observe(ctx, []managed.Ref{ref})
		if err != nil {
			return false, err
		}
		return views[0].GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, nil
	}
	u, err := e.Objects.Read(ctx, ref)
	if err != nil || u == nil {
		return false, err
	}
	value, _, _ := unstructured.NestedString(u.Object, cond.path...)
	return value == cond.value, nil
}

func key(r managed.Ref) string { return r.APIVersion + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name }
