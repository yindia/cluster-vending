package kubestore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	platformv1alpha1 "github.com/yindia/cluster-vending/pkg/apis/platform/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

type clusters struct{ store }

// networkDeleting is the stored state of a network being deleted.
const networkDeleting = "NETWORK_STATE_DELETING"

func clusterAccess(obj *platformv1alpha1.Cluster) rowAccess {
	return rowAccess{row: func() *platformv1alpha1.Row { return &obj.Spec.Row }, tenant: func() string { return obj.Spec.TenantID }}
}

// clusterKeys are the claims of a cluster: its name per account and
// region (the cloud cluster's name; clusters without an account, stored
// before accounts were recorded, claim none) and its create request ID
// per tenant.
func clusterKeys(c *model.Cluster) []string {
	var keys []string
	if c.CloudAccountID != "" {
		keys = append(keys, fmt.Sprintf("cluster-name/%s/%s/%s", c.CloudAccountID, c.Region, c.Name))
	}
	if c.CreateRequestID != nil {
		keys = append(keys, fmt.Sprintf("cluster-request/%s/%s", c.TenantID, *c.CreateRequestID))
	}
	return keys
}

// Create stores a cluster in its network and claims the network: the
// network must not be DELETING, and its etag changes. DeleteNetwork marks
// the network DELETING with the etag it read before it checked for
// clusters, so of a create and a delete that race exactly one wins.
//
// Without a transaction the order does the work: the cluster is stored
// first, then the network is claimed. A delete that looked for clusters
// before this one was stored still holds the old etag and is refused; a
// delete that already won leaves the network DELETING (or gone), which
// the claim sees, and the cluster is taken back.
func (r clusters) Create(ctx context.Context, c *model.Cluster) error {
	network := &platformv1alpha1.Network{}
	if err := r.get(ctx, c.NetworkID, network); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	if n := network.Spec; n.TenantID != c.TenantID || n.Provider != string(c.Provider) {
		return fmt.Errorf("%w: network %s belongs to tenant %s on %s, cluster to tenant %s on %s",
			repository.ErrInvalid, n.Name, n.TenantID, n.Provider, c.TenantID, c.Provider)
	}
	if network.Spec.State == networkDeleting {
		return fmt.Errorf("%w: network %s is being deleted", repository.ErrConflict, network.Name)
	}
	if err := invalid(c.Provider.Validate()); err != nil {
		return err
	}
	// The network's account is the cluster's: with it the name is claimed
	// per account and region.
	c.CloudAccountID = network.Spec.CloudAccountID
	if err := prepare(&c.Resource); err != nil {
		return err
	}
	spec, err := toCluster(c)
	if err != nil {
		return err
	}
	obj := &platformv1alpha1.Cluster{ObjectMeta: r.meta(c.ID, clusterLabels(c)), Spec: spec}
	keys := clusterKeys(c)
	if err := r.insert(ctx, kindCluster, obj, keys...); err != nil {
		return err
	}
	if err := r.claimNetwork(ctx, c.NetworkID); err != nil {
		if rerr := r.remove(ctx, obj); rerr != nil && !errors.Is(rerr, repository.ErrNotFound) {
			return errors.Join(err, fmt.Errorf("take the cluster back: %w", rerr))
		}
		r.release(ctx, keys...)
		return err
	}
	return nil
}

// claimNetwork gives the network a new etag unless it is DELETING
// (ErrConflict) or gone (ErrNotFound). It is not conditional on an etag
// read before: two creates in one network, or the loop touching the
// network meanwhile, must not fail each other.
func (r clusters) claimNetwork(ctx context.Context, id string) error {
	network := &platformv1alpha1.Network{}
	err := r.mutate(ctx, id, network, func() error {
		if network.Spec.State == networkDeleting {
			return fmt.Errorf("%w: network %s is being deleted", repository.ErrConflict, id)
		}
		network.Spec.ETag = newETag()
		return nil
	})
	if errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("network: %w", err)
	}
	return err
}

func (r clusters) Get(ctx context.Context, tenantID, id string) (*model.Cluster, error) {
	obj := &platformv1alpha1.Cluster{}
	if err := r.get(ctx, id, obj); err != nil {
		return nil, err
	}
	if obj.Spec.TenantID != tenantID {
		return nil, repository.ErrNotFound
	}
	return fromCluster(obj), nil
}

func (r clusters) GetByCreateRequest(ctx context.Context, tenantID, requestID string) (*model.Cluster, error) {
	id, err := r.holder(ctx, fmt.Sprintf("cluster-request/%s/%s", tenantID, requestID))
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, tenantID, id)
}

// all lists the clusters with labels, ordered by ID (oldest first).
func (r clusters) all(ctx context.Context, labels map[string]string) ([]model.Cluster, error) {
	list := &platformv1alpha1.ClusterList{}
	if err := r.list(ctx, list, labels); err != nil {
		return nil, err
	}
	out := make([]model.Cluster, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, *fromCluster(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r clusters) List(ctx context.Context, tenantID string, p repository.Page) ([]model.Cluster, error) {
	if p.Limit <= 0 {
		return nil, fmt.Errorf("page limit must be positive, got %d", p.Limit)
	}
	rows, err := r.all(ctx, map[string]string{platformv1alpha1.LabelTenant: tenantID})
	if err != nil {
		return nil, err
	}
	return page(rows, func(c model.Cluster) string { return c.ID }, p)
}

func (r clusters) ListByNetwork(ctx context.Context, networkID string) ([]model.Cluster, error) {
	return r.all(ctx, map[string]string{platformv1alpha1.LabelNetwork: networkID})
}

func (r clusters) ListByName(ctx context.Context, tenantID, name string) ([]model.Cluster, error) {
	rows, err := r.all(ctx, map[string]string{platformv1alpha1.LabelTenant: tenantID})
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, c := range rows {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r clusters) Update(ctx context.Context, c *model.Cluster) error {
	obj := &platformv1alpha1.Cluster{}
	return r.update(ctx, &c.Resource, obj, clusterAccess(obj), func() error {
		spec, err := toCluster(c)
		obj.Spec, obj.Labels = spec, clusterLabels(c)
		return err
	})
}

func (r clusters) SetStatus(ctx context.Context, tenantID, id, etag string, status model.JSON) error {
	obj := &platformv1alpha1.Cluster{}
	return r.setStatus(ctx, tenantID, id, etag, status, obj, clusterAccess(obj))
}

func (r clusters) MarkBehind(ctx context.Context, tenantID, id string) error {
	obj := &platformv1alpha1.Cluster{}
	return r.markBehind(ctx, tenantID, id, obj, clusterAccess(obj))
}

// Delete removes the cluster's node pools, then the cluster.
func (r clusters) Delete(ctx context.Context, tenantID, id string) error {
	obj := &platformv1alpha1.Cluster{}
	if err := r.get(ctx, id, obj); err != nil {
		return err
	}
	if obj.Spec.TenantID != tenantID {
		return repository.ErrNotFound
	}
	pools := &platformv1alpha1.NodePoolList{}
	if err := r.list(ctx, pools, map[string]string{platformv1alpha1.LabelCluster: id}); err != nil {
		return err
	}
	for i := range pools.Items {
		if err := nodePools(r).drop(ctx, &pools.Items[i]); err != nil {
			return err
		}
	}
	if err := r.remove(ctx, obj); err != nil {
		return err
	}
	r.release(ctx, clusterKeys(fromCluster(obj))...)
	return nil
}

func (r clusters) ListPending(ctx context.Context, states []string, renderTarget int64) ([]model.Cluster, error) {
	rows, err := r.all(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, c := range rows {
		if isPending(&c.Resource, states, renderTarget) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r clusters) CountByState(ctx context.Context) (map[string]int64, error) {
	rows, err := r.all(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, c := range rows {
		out[c.State]++
	}
	return out, nil
}
