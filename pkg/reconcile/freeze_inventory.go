package reconcile

import (
	"context"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

func (d Deps) FreezeTargets(ctx context.Context) (map[string]FreezeTarget, error) {
	networks := &cloudv1alpha1.NetworkList{}
	clusters := &cloudv1alpha1.ClusterList{}
	if err := d.Reader().List(ctx, networks); err != nil {
		return nil, err
	}
	if err := d.Reader().List(ctx, clusters); err != nil {
		return nil, err
	}
	targets := map[string]FreezeTarget{}
	byName := map[client.ObjectKey]string{}
	for _, network := range networks.Items {
		id := network.Labels["cloud.cluster-vending.io/id"]
		if id == "" {
			continue
		}
		targets[id] = FreezeTarget{TenantID: strings.TrimPrefix(network.Namespace, "tenant-"), NetworkID: id}
		byName[client.ObjectKeyFromObject(&network)] = id
	}
	for _, cluster := range clusters.Items {
		id := cluster.Labels["cloud.cluster-vending.io/id"]
		if id == "" {
			continue
		}
		networkID := byName[client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Spec.Network.NetworkRef.Name}]
		targets[id] = FreezeTarget{TenantID: strings.TrimPrefix(cluster.Namespace, "tenant-"), NetworkID: networkID, ClusterID: id}
	}
	return targets, nil
}

func HeldFreezeScope(hold *cloudv1alpha1.ReleaseHold, targets map[string]FreezeTarget, now time.Time) PauseScope {
	return PauseScope{Targets: targets, Select: func(target FreezeTarget) bool { return FreezeForTarget(hold, now, target).Holds() }}
}
