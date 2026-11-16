package management

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/rollout"
)

// Admission lists are cached plans. Check their tier against the parent
// network read from the API server before starting a new change.
func (p platformSource) currentAdmissionTier(ctx context.Context, channel *cloud.ReleaseChannel, plan *cloud.ChannelReleaseMoves, tenant, name string) (bool, string, error) {
	if channel.Spec.Paused {
		return false, "the channel is paused", nil
	}
	cluster := &cloud.Cluster{}
	key := client.ObjectKey{Namespace: kube.TenantNamespace(tenant), Name: name}
	if err := p.c.Get(ctx, key, cluster); err != nil {
		return false, "", err
	}
	if !strings.EqualFold(string(cluster.Spec.ReleaseChannel), channel.Name) {
		return false, "the cluster's release channel changed; waiting for a new rollout plan", nil
	}
	if cluster.Spec.Network.NetworkRef.Name == "" {
		return false, "the cluster has no parent network; cannot determine rollout tier", nil
	}
	network := &cloud.Network{}
	key.Name = cluster.Spec.Network.NetworkRef.Name
	if err := p.c.Get(ctx, key, network); err != nil {
		return false, "", fmt.Errorf("read network tier: %w", err)
	}
	if plan == nil || plan.Tier == "" || rollout.Tier(network.Spec.Tier) != rollout.Tier(plan.Tier) {
		return false, "the network tier changed; waiting for a new rollout plan", nil
	}
	return true, "", nil
}
