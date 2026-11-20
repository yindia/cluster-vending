package rollout

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// Read reads what planning ch's rollout needs: the release
// it offers (nil when none or gone), its member clusters and every release
// hold. Parent lookup failures are isolated to that channel.
func Read(ctx context.Context, c client.Reader, ch *cloudv1alpha1.ReleaseChannel) (Input, error) {
	in := Input{Channel: ch}
	if ch.Status.CodeChanges != nil {
		in.CodeEpoch = ch.Status.CodeChanges.Release
	}
	if ch.Status.Release != "" {
		rel := &cloudv1alpha1.Release{}
		switch err := c.Get(ctx, client.ObjectKey{Name: ch.Status.Release}, rel); {
		case err == nil:
			in.Release = rel
		case client.IgnoreNotFound(err) != nil:
			return in, fmt.Errorf("read release %s: %w", ch.Status.Release, err)
		}
	}
	clusters := &cloudv1alpha1.ClusterList{}
	if err := c.List(ctx, clusters); err != nil {
		return in, fmt.Errorf("list clusters: %w", err)
	}
	holds := &cloudv1alpha1.ReleaseHoldList{}
	if err := c.List(ctx, holds); err != nil {
		return in, fmt.Errorf("list release holds: %w", err)
	}
	networks := &cloudv1alpha1.NetworkList{}
	if err := c.List(ctx, networks); err != nil {
		return in, fmt.Errorf("list network tiers: %w", err)
	}
	byName := map[client.ObjectKey]*cloudv1alpha1.Network{}
	in.NetworkClusters = slices.Clone(clusters.Items)
	boundChannels := map[client.ObjectKey]map[string]bool{}
	for _, cluster := range clusters.Items {
		if !cluster.DeletionTimestamp.IsZero() || cluster.Spec.ReleaseChannel == "" {
			continue
		}
		key := client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Spec.Network.NetworkRef.Name}
		if boundChannels[key] == nil {
			boundChannels[key] = map[string]bool{}
		}
		boundChannels[key][strings.ToLower(string(cluster.Spec.ReleaseChannel))] = true
	}
	for index := range networks.Items {
		network := &networks.Items[index]
		key := client.ObjectKeyFromObject(network)
		byName[key] = network
		if len(boundChannels[key]) == 0 || boundChannels[key][ch.Name] {
			in.Networks = append(in.Networks, *network)
		}
	}
	clusters.Items = slices.DeleteFunc(clusters.Items, func(cluster cloudv1alpha1.Cluster) bool {
		return !strings.EqualFold(string(cluster.Spec.ReleaseChannel), ch.Name)
	})
	for index := range clusters.Items {
		cluster := &clusters.Items[index]
		network := byName[client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Spec.Network.NetworkRef.Name}]
		cluster.Status.Tier = cloudv1alpha1.ClusterTier0
		if network != nil {
			if network.Spec.Tier != "" {
				cluster.Status.Tier = network.Spec.Tier
			}
			if cluster.Labels == nil {
				cluster.Labels = map[string]string{}
			}
			cluster.Labels["cluster-vending.io/network"] = network.Labels["cloud.cluster-vending.io/id"]
		} else if cluster.Spec.Network.NetworkRef.Name != "" {
			return in, fmt.Errorf("network %s of cluster %s/%s is missing; cannot determine rollout tier", cluster.Spec.Network.NetworkRef.Name, cluster.Namespace, cluster.Name)
		}
	}
	components := &cloudv1alpha1.ComponentList{}
	if err := c.List(ctx, components); err != nil {
		return in, fmt.Errorf("list tenant components: %w", err)
	}
	in.Components = components.Items
	in.Clusters, in.Holds = clusters.Items, holds.Items
	return in, nil
}
