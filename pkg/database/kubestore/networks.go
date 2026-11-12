package kubestore

import (
	"context"
	"fmt"
	"sort"

	platformv1alpha1 "github.com/yindia/cluster-vending/pkg/apis/platform/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

type networks struct{ store }

func networkAccess(obj *platformv1alpha1.Network) rowAccess {
	return rowAccess{row: func() *platformv1alpha1.Row { return &obj.Spec.Row }, tenant: func() string { return obj.Spec.TenantID }}
}

// networkKeys are the claims of a network: its create request ID per
// tenant, and the cloud network it was imported from per account and
// region. A network without them claims nothing.
func networkKeys(n *model.Network) []string {
	var keys []string
	if n.CreateRequestID != nil {
		keys = append(keys, fmt.Sprintf("network-request/%s/%s", n.TenantID, *n.CreateRequestID))
	}
	if n.ExternalID != nil {
		keys = append(keys, fmt.Sprintf("network-external/%s/%s/%s", n.CloudAccountID, n.Region, *n.ExternalID))
	}
	return keys
}

func (r networks) Create(ctx context.Context, n *model.Network) error {
	if err := r.get(ctx, n.TenantID, &platformv1alpha1.Tenant{}); err != nil {
		return err
	}
	account := &platformv1alpha1.CloudAccount{}
	if err := r.get(ctx, n.CloudAccountID, account); err != nil {
		return fmt.Errorf("cloud account: %w", err)
	}
	if a := account.Spec; a.TenantID != n.TenantID || a.Provider != string(n.Provider) {
		return fmt.Errorf("%w: account %s belongs to tenant %s on %s, network to tenant %s on %s",
			repository.ErrInvalid, a.AccountID, a.TenantID, a.Provider, n.TenantID, n.Provider)
	}
	if err := invalid(n.Provider.Validate()); err != nil {
		return err
	}
	if err := prepare(&n.Resource); err != nil {
		return err
	}
	spec, err := toNetwork(n)
	if err != nil {
		return err
	}
	obj := &platformv1alpha1.Network{ObjectMeta: r.meta(n.ID, networkLabels(n)), Spec: spec}
	return r.insert(ctx, kindNetwork, obj, networkKeys(n)...)
}

func (r networks) Get(ctx context.Context, tenantID, id string) (*model.Network, error) {
	obj := &platformv1alpha1.Network{}
	if err := r.get(ctx, id, obj); err != nil {
		return nil, err
	}
	if obj.Spec.TenantID != tenantID {
		return nil, repository.ErrNotFound
	}
	return fromNetwork(obj), nil
}

func (r networks) GetByCreateRequest(ctx context.Context, tenantID, requestID string) (*model.Network, error) {
	id, err := r.holder(ctx, fmt.Sprintf("network-request/%s/%s", tenantID, requestID))
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, tenantID, id)
}

// all lists the networks with labels, ordered by ID (oldest first).
func (r networks) all(ctx context.Context, labels map[string]string) ([]model.Network, error) {
	list := &platformv1alpha1.NetworkList{}
	if err := r.list(ctx, list, labels); err != nil {
		return nil, err
	}
	out := make([]model.Network, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, *fromNetwork(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r networks) List(ctx context.Context, tenantID string, p repository.Page) ([]model.Network, error) {
	if p.Limit <= 0 {
		return nil, fmt.Errorf("page limit must be positive, got %d", p.Limit)
	}
	rows, err := r.all(ctx, map[string]string{platformv1alpha1.LabelTenant: tenantID})
	if err != nil {
		return nil, err
	}
	return page(rows, func(n model.Network) string { return n.ID }, p)
}

func (r networks) Update(ctx context.Context, n *model.Network) error {
	obj := &platformv1alpha1.Network{}
	return r.update(ctx, &n.Resource, obj, networkAccess(obj), func() error {
		spec, err := toNetwork(n)
		obj.Spec, obj.Labels = spec, networkLabels(n)
		return err
	})
}

func (r networks) SetStatus(ctx context.Context, tenantID, id, etag string, status model.JSON) error {
	obj := &platformv1alpha1.Network{}
	return r.setStatus(ctx, tenantID, id, etag, status, obj, networkAccess(obj))
}

func (r networks) MarkBehind(ctx context.Context, tenantID, id string) error {
	obj := &platformv1alpha1.Network{}
	return r.markBehind(ctx, tenantID, id, obj, networkAccess(obj))
}

// Delete removes the network unless clusters use it. A cluster created in
// the same instant sees the network gone when it claims it, and takes
// itself back (clusters.Create).
func (r networks) Delete(ctx context.Context, tenantID, id string) error {
	obj := &platformv1alpha1.Network{}
	if err := r.get(ctx, id, obj); err != nil {
		return err
	}
	if obj.Spec.TenantID != tenantID {
		return repository.ErrNotFound
	}
	users := &platformv1alpha1.ClusterList{}
	if err := r.list(ctx, users, map[string]string{platformv1alpha1.LabelNetwork: id}); err != nil {
		return err
	}
	if n := len(users.Items); n > 0 {
		return fmt.Errorf("%w: %d live clusters reference it", repository.ErrInUse, n)
	}
	if err := r.remove(ctx, obj); err != nil {
		return err
	}
	r.release(ctx, networkKeys(fromNetwork(obj))...)
	return nil
}

func (r networks) ListPending(ctx context.Context, states []string, renderTarget int64) ([]model.Network, error) {
	rows, err := r.all(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, n := range rows {
		if isPending(&n.Resource, states, renderTarget) {
			out = append(out, n)
		}
	}
	return out, nil
}
