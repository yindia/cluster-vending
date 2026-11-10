// Package nodepool implements the cloud.v1 NodePoolService.
package nodepool

import (
	"context"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// NodePoolService implements cloud.v1.NodePoolService. Every RPC returns an
// empty response for now.
type NodePoolService struct{ deps service.Deps }

var _ cloudv1connect.NodePoolServiceHandler = (*NodePoolService)(nil)

// NewNodePoolService returns the NodePoolService.
func NewNodePoolService(d service.Deps) *NodePoolService { return &NodePoolService{deps: d} }

func (s *NodePoolService) CreateNodePool(context.Context, *cloudv1.CreateNodePoolRequest) (*cloudv1.CreateNodePoolResponse, error) {
	return &cloudv1.CreateNodePoolResponse{}, nil
}

func (s *NodePoolService) GetNodePool(context.Context, *cloudv1.GetNodePoolRequest) (*cloudv1.GetNodePoolResponse, error) {
	return &cloudv1.GetNodePoolResponse{}, nil
}

func (s *NodePoolService) ListNodePools(context.Context, *cloudv1.ListNodePoolsRequest) (*cloudv1.ListNodePoolsResponse, error) {
	return &cloudv1.ListNodePoolsResponse{}, nil
}

func (s *NodePoolService) UpdateNodePool(context.Context, *cloudv1.UpdateNodePoolRequest) (*cloudv1.UpdateNodePoolResponse, error) {
	return &cloudv1.UpdateNodePoolResponse{}, nil
}

func (s *NodePoolService) DeleteNodePool(context.Context, *cloudv1.DeleteNodePoolRequest) (*cloudv1.DeleteNodePoolResponse, error) {
	return &cloudv1.DeleteNodePoolResponse{}, nil
}
