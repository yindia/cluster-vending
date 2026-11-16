package admin

import (
	"context"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

func (s *Service) clusters(ctx context.Context) ([]cloudv1alpha1.Cluster, error) {
	list := &cloudv1alpha1.ClusterList{}
	if err := s.reader().List(ctx, list); err != nil {
		return nil, s.readError(ctx, "clusters", err)
	}
	return list.Items, nil
}
func (s *Service) networks(ctx context.Context) ([]cloudv1alpha1.Network, error) {
	list := &cloudv1alpha1.NetworkList{}
	if err := s.reader().List(ctx, list); err != nil {
		return nil, s.readError(ctx, "networks", err)
	}
	return list.Items, nil
}
