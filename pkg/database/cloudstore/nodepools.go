package cloudstore

import (
	"context"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// nodePools refuses every call with ErrNodePools: the kinds have no node
// pool (a cluster's default one is spec.defaultNodePool of its Cluster),
// and nothing in the API or the reconcilers stores node pool rows.
type nodePools struct{}

func (nodePools) Create(context.Context, *model.NodePool) error { return ErrNodePools }

func (nodePools) Get(context.Context, string, string) (*model.NodePool, error) {
	return nil, ErrNodePools
}

func (nodePools) List(context.Context, string, repository.Page) ([]model.NodePool, error) {
	return nil, ErrNodePools
}

func (nodePools) Update(context.Context, *model.NodePool) error { return ErrNodePools }

func (nodePools) Delete(context.Context, string, string) error { return ErrNodePools }
