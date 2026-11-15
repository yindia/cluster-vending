package managed

import (
	"context"
	"errors"
	"fmt"
	"slices"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// UsageKind is Crossplane's protection.crossplane.io Usage.
const UsageKind = "Usage"

// Delete asks the management cluster to delete objects, last first
// (dependents come after what they use). Crossplane removes the cloud
// resources, then the objects. It skips:
//   - Usages: Kubernetes garbage-collects each with the object using
//     (Crossplane owner-references it to spec.by); deleting one first
//     would release its protection at once.
//   - objects already gone.
//
// A 409 is the Usage webhook refusing an object still in use: Crossplane
// records the attempt and replays the delete once the user is gone
// (replayDeletion), so it counts as done. Other errors are collected; the
// remaining objects are still deleted. An object the platform protects
// from deletion is switched to full management first (allowDelete): its
// cloud resource goes too.
func (c Client) Delete(ctx context.Context, refs []Ref) error {
	return c.delete(ctx, refs, true)
}

// delete is Delete; release switches protected objects to full management
// first, without it they are deleted as they are.
func (c Client) delete(ctx context.Context, refs []Ref, release bool) error {
	var errs []error
	for i := len(refs) - 1; i >= 0; i-- {
		r := refs[i]
		if r.Kind == UsageKind {
			continue
		}
		if release {
			if err := c.allowDelete(ctx, r); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		err := c.Kube.Delete(ctx, r.empty())
		switch {
		case err == nil, apierrors.IsNotFound(err), meta.IsNoMatchError(err), apierrors.IsConflict(err):
		default:
			errs = append(errs, fmt.Errorf("delete %s %s: %w", r.Kind, r.Name, err))
		}
	}
	return errors.Join(errs...)
}

// DeleteDropped deletes one object a render no longer contains. Unlike
// Delete it deletes Usages too: a dropped Usage must stop protecting its
// object. In an apply marked KeepDropped (a code change) a protected
// object is deleted as it is: its cloud resource stays.
func (c Client) DeleteDropped(ctx context.Context, r Ref) error {
	if r.Kind != UsageKind {
		return c.delete(ctx, []Ref{r}, !keepsDropped(ctx))
	}
	if err := c.Kube.Delete(ctx, r.empty()); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return fmt.Errorf("delete %s %s: %w", r.Kind, r.Name, err)
	}
	return nil
}

// DeleteOrder is the order an owner's objects are deleted in: tier by tier
// (kinds), each once every earlier tier is gone, so the cloud deletes
// dependents before what they depend on (node groups before their EKS
// cluster before its IAM roles; NAT gateways before their EIPs and
// subnets). Kinds in no tier go first.
type DeleteOrder [][]string

func (o DeleteOrder) tier(kind string) int {
	for i, kinds := range o {
		if slices.Contains(kinds, kind) {
			return i + 1
		}
	}
	return 0
}

// DeleteNext deletes the objects of the first tier that still has objects
// (views not MISSING; views are refs' views, as Observe returns them) and
// reports how many objects remain in all tiers. Calling it again deletes
// nothing new until that tier is gone; it is idempotent.
func (c Client) DeleteNext(ctx context.Context, refs []Ref, views []*cloudv1.ManagedResource, order DeleteOrder) (int, error) {
	gone := make(map[string]bool, len(views))
	for _, v := range views {
		if v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			gone[v.GetKind()+"/"+v.GetName()] = true
		}
	}
	remaining := make(map[int][]Ref)
	left := 0
	for _, r := range refs {
		if gone[r.Kind+"/"+r.Name] {
			continue
		}
		t := order.tier(r.Kind)
		remaining[t] = append(remaining[t], r)
		left++
	}
	for t := 0; t <= len(order); t++ {
		if len(remaining[t]) == 0 {
			continue
		}
		if err := c.Delete(ctx, remaining[t]); err != nil {
			return left, fmt.Errorf("delete tier %d: %w", t, err)
		}
		break
	}
	return left, nil
}

// Current is the tier being deleted: the kinds of the first tier that
// still has objects in views; nil when everything is gone.
func (o DeleteOrder) Current(views []*cloudv1.ManagedResource) []string {
	first := -1
	for _, v := range views {
		if v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			continue
		}
		if t := o.tier(v.GetKind()); first < 0 || t < first {
			first = t
		}
	}
	switch {
	case first < 0:
		return nil
	case first == 0:
		var kinds []string
		for _, v := range views {
			if o.tier(v.GetKind()) == 0 && v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING &&
				!slices.Contains(kinds, v.GetKind()) {
				kinds = append(kinds, v.GetKind())
			}
		}
		return kinds
	default:
		return o[first-1]
	}
}

// ReleaseOrphanUsages deletes the Usages among refs whose "by" object is
// gone but that are still there: Kubernetes garbage-collects a Usage only
// once Crossplane owner-referenced it to its user, which never happens
// when the user was deleted (or never created) first. Deleting the Usage
// releases its object at once. views are refs' views, as Observe returns
// them.
func (c Client) ReleaseOrphanUsages(ctx context.Context, refs []Ref, views []*cloudv1.ManagedResource) error {
	present := make(map[string]bool, len(views))
	for _, v := range views {
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			present[v.GetKind()+"/"+v.GetName()] = true
		}
	}
	var errs []error
	for _, r := range refs {
		if r.Kind != UsageKind || !present[r.Kind+"/"+r.Name] {
			continue
		}
		u := &protectionv1beta1.Usage{}
		if err := c.Reader().Get(ctx, r.key(), u); err != nil {
			errs = append(errs, client.IgnoreNotFound(err))
			continue
		}
		by := u.Spec.By
		if by == nil || by.ResourceRef == nil || present[by.Kind+"/"+by.ResourceRef.Name] {
			continue
		}
		if err := client.IgnoreNotFound(c.Kube.Delete(ctx, u)); err != nil {
			errs = append(errs, fmt.Errorf("release usage %s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}
