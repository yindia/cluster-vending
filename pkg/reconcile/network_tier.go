package reconcile

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func (d Deps) NetworkTier(ctx context.Context, tenantID, networkID string) (cloudv1.ClusterTier, error) {
	network, err := d.Store.Networks().Get(ctx, tenantID, networkID)
	if err != nil {
		return 0, fmt.Errorf("read network tier: %w", err)
	}
	spec := &cloudv1.NetworkSpec{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(network.Spec, spec); err != nil {
		return 0, fmt.Errorf("read network tier: %w", err)
	}
	if spec.GetTier() == cloudv1.Tier_TIER_UNSPECIFIED {
		return cloudv1.ClusterTier_CLUSTER_TIER_0, nil
	}
	return cloudv1.ClusterTier(spec.GetTier()), nil
}
