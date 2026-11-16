package management

import (
	"context"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/rollout"
)

// ClusterCodeAdmitted implements reconcile.CodeAdmitter: the channel's
// code rollout (status.codeChanges, which the rollout controller plans)
// lists the cluster among those that may apply their code change now.
func (p platformSource) ClusterCodeAdmitted(ctx context.Context, channel, tenantID, cluster string) (bool, string, error) {
	ch := &cloudv1alpha1.ReleaseChannel{}
	if err := p.c.Get(ctx, client.ObjectKey{Name: channel}, ch); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("release channel %s does not exist", channel), nil
		}
		return false, "", err
	}
	admitted, why := codeAdmittedBy(ch.Status.CodeChanges, kube.TenantNamespace(tenantID)+"/"+cluster)
	if !admitted {
		return false, why, nil
	}
	return p.currentAdmissionTier(ctx, ch, ch.Status.CodeChanges, tenantID, cluster)
}

// codeAdmittedBy reads a channel's code rollout for the cluster key
// (namespace/name); why it waits when it is not admitted.
func codeAdmittedBy(m *cloudv1alpha1.ChannelReleaseMoves, key string) (bool, string) {
	switch {
	case m == nil:
		return false, "waiting for the channel's code rollout to be planned"
	case m.Halted:
		return false, "the channel's code rollout is halted: " + m.Waiting
	case slices.Contains(m.Admitted, key):
		return true, ""
	case m.Waiting != "":
		return false, "waiting its turn in the channel's code rollout: " + m.Waiting
	}
	return false, "waiting its turn in the channel's code rollout"
}

// NetworkCodeAdmitted implements reconcile.CodeAdmitter for a network: no
// freeze holds it, and the code rollout of each channel of its clusters
// has reached the network's tier (rollout.NetworkCodeAdmitted).
func (p platformSource) NetworkCodeAdmitted(ctx context.Context, tenantID, networkID string) (bool, string, error) {
	network, clusters, err := p.networkClusters(ctx, tenantID, networkID)
	if err != nil {
		return false, "", err
	}
	channels := &cloudv1alpha1.ReleaseChannelList{}
	if err := p.c.List(ctx, channels); err != nil {
		return false, "", fmt.Errorf("list release channels: %w", err)
	}
	byName := make(map[string]*cloudv1alpha1.ReleaseChannel, len(channels.Items))
	for i := range channels.Items {
		byName[channels.Items[i].Name] = &channels.Items[i]
	}
	holds, err := p.Holds(ctx)
	if err != nil {
		return false, "", err
	}
	admitted, why := rollout.NetworkCodeAdmitted(network, clusters, byName, holds, time.Now())
	return admitted, why, nil
}

// networkClusters are the clusters of the tenant on the network, those
// being deleted left out.
func (p platformSource) networkClusters(ctx context.Context, tenantID, networkID string) (*cloudv1alpha1.Network, []cloudv1alpha1.Cluster, error) {
	ns := client.InNamespace(kube.TenantNamespace(tenantID))
	networks := &cloudv1alpha1.NetworkList{}
	if err := p.c.List(ctx, networks, ns, client.MatchingLabels{cloudstore.LabelID: networkID}); err != nil {
		return nil, nil, fmt.Errorf("read network %s: %w", networkID, err)
	}
	if len(networks.Items) == 0 {
		return nil, nil, fmt.Errorf("network %s is missing; cannot determine rollout tier", networkID)
	}
	name := networks.Items[0].Name
	list := &cloudv1alpha1.ClusterList{}
	if err := p.c.List(ctx, list, ns); err != nil {
		return nil, nil, fmt.Errorf("list the clusters of network %s: %w", networkID, err)
	}
	for index := range list.Items {
		list.Items[index].Status.Tier = networks.Items[0].Spec.Tier
	}
	return &networks.Items[0], slices.DeleteFunc(list.Items, func(c cloudv1alpha1.Cluster) bool {
		return !c.DeletionTimestamp.IsZero() || c.Spec.Network.NetworkRef.Name != name
	}), nil
}
