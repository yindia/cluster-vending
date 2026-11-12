package kubestore

import (
	"context"
	"fmt"
	"sort"

	platformv1alpha1 "github.com/yindia/cluster-vending/pkg/apis/platform/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

type nodePools struct{ store }

func nodePoolAccess(obj *platformv1alpha1.NodePool) rowAccess {
	return rowAccess{row: func() *platformv1alpha1.Row { return &obj.Spec.Row }, tenant: func() string { return "" }}
}

// nodePoolKeys are the claims of a node pool: its name and its create
// request ID, both per cluster.
func nodePoolKeys(p *model.NodePool) []string {
	keys := []string{fmt.Sprintf("node-pool-name/%s/%s", p.ClusterID, p.Name)}
	if p.CreateRequestID != nil {
		keys = append(keys, fmt.Sprintf("node-pool-request/%s/%s", p.ClusterID, *p.CreateRequestID))
	}
	return keys
}

func nodePoolLabels(p *model.NodePool) map[string]string {
	return map[string]string{platformv1alpha1.LabelCluster: p.ClusterID}
}

func (r nodePools) Create(ctx context.Context, p *model.NodePool) error {
	if err := r.get(ctx, p.ClusterID, &platformv1alpha1.Cluster{}); err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	if err := prepare(&p.Resource); err != nil {
		return err
	}
	spec, err := toNodePool(p)
	if err != nil {
		return err
	}
	obj := &platformv1alpha1.NodePool{ObjectMeta: r.meta(p.ID, nodePoolLabels(p)), Spec: spec}
	return r.insert(ctx, kindNodePool, obj, nodePoolKeys(p)...)
}

// find is the cluster's node pool of the name; ErrNotFound without one.
func (r nodePools) find(ctx context.Context, clusterID, name string) (*platformv1alpha1.NodePool, error) {
	list := &platformv1alpha1.NodePoolList{}
	if err := r.list(ctx, list, map[string]string{platformv1alpha1.LabelCluster: clusterID}); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.Name == name {
			return &list.Items[i], nil
		}
	}
	return nil, repository.ErrNotFound
}

func (r nodePools) Get(ctx context.Context, clusterID, name string) (*model.NodePool, error) {
	obj, err := r.find(ctx, clusterID, name)
	if err != nil {
		return nil, err
	}
	return fromNodePool(obj), nil
}

func (r nodePools) List(ctx context.Context, clusterID string, p repository.Page) ([]model.NodePool, error) {
	if p.Limit <= 0 {
		return nil, fmt.Errorf("page limit must be positive, got %d", p.Limit)
	}
	list := &platformv1alpha1.NodePoolList{}
	if err := r.list(ctx, list, map[string]string{platformv1alpha1.LabelCluster: clusterID}); err != nil {
		return nil, err
	}
	rows := make([]model.NodePool, 0, len(list.Items))
	for i := range list.Items {
		rows = append(rows, *fromNodePool(&list.Items[i]))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return page(rows, func(n model.NodePool) string { return n.Name }, p)
}

func (r nodePools) Update(ctx context.Context, p *model.NodePool) error {
	obj := &platformv1alpha1.NodePool{}
	return r.update(ctx, &p.Resource, obj, nodePoolAccess(obj), func() error {
		spec, err := toNodePool(p)
		obj.Spec, obj.Labels = spec, nodePoolLabels(p)
		return err
	})
}

func (r nodePools) Delete(ctx context.Context, clusterID, name string) error {
	obj, err := r.find(ctx, clusterID, name)
	if err != nil {
		return err
	}
	return r.drop(ctx, obj)
}

// drop deletes a node pool and releases its claims.
func (r nodePools) drop(ctx context.Context, obj *platformv1alpha1.NodePool) error {
	if err := r.remove(ctx, obj); err != nil {
		return err
	}
	r.release(ctx, nodePoolKeys(fromNodePool(obj))...)
	return nil
}
