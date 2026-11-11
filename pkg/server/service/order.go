package service

import (
	"context"
	"fmt"
	"slices"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

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
// (views not MISSING) and reports how many objects remain in all tiers.
// Calling it again deletes nothing new until that tier is gone; it is
// idempotent, so every status read of a deleting owner can call it.
func (d Deps) DeleteNext(ctx context.Context, records []model.ManagedResource, views []*cloudv1.ManagedResource,
	order DeleteOrder,
) (int, error) {
	gone := make(map[string]bool, len(views))
	for _, v := range views {
		if v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			gone[v.GetKind()+"/"+v.GetName()] = true
		}
	}
	remaining := make(map[int][]model.ManagedResource)
	left := 0
	for _, r := range records {
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
		if err := d.DeleteRecorded(ctx, remaining[t]); err != nil {
			return left, fmt.Errorf("delete tier %d: %w", t, err)
		}
		break
	}
	return left, nil
}

// OwnerGone reports whether every recorded object of an owner is gone
// from the management cluster.
func (d Deps) OwnerGone(ctx context.Context, kind model.OwnerKind, id string) (bool, error) {
	records, err := d.Store.ManagedResources().ListByOwner(ctx, kind, id)
	if err != nil {
		return false, err
	}
	views, err := d.Observe(ctx, records)
	if err != nil {
		return false, err
	}
	return AllGone(views), nil
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
