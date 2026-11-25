package cluster

import (
	"context"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// platformCanStart reports whether the platform components may be
// installed for the first time. A RUNNING cluster may. So may a cluster
// still being created (or changed before it ever ran) once it can take
// Helm charts: its EKS cluster, a node group and its own ClusterAuth (the
// kubeconfig) are Ready.
//
// Waiting for RUNNING instead would deadlock a cluster with an add-on that
// does not tolerate the system node group's taint (metrics-server,
// kube-state-metrics, cert-manager): its pods wait for Karpenter's nodes,
// Karpenter waits for RUNNING, and RUNNING waits for the add-on.
func (s *Reconciler) platformCanStart(ctx context.Context, row *model.Cluster) (bool, error) {
	switch row.State {
	case StateRunning:
		return true, nil
	case StateProvisioning, StateUpdating:
	default:
		return false, nil
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return false, err
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return false, err
	}
	return servesCharts(crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name), views), nil
}

// servesCharts reports whether views hold a Ready EKS cluster, a Ready node
// group and a Ready ClusterAuth of the cluster's own (not the live
// reader's, which an account without the reader stack never gets Ready).
func servesCharts(prefix string, views []*cloudv1.ManagedResource) bool {
	var eks, nodes, auth bool
	for _, v := range views {
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY {
			continue
		}
		switch v.GetKind() {
		case awseks.Cluster_Kind:
			eks = true
		case awseks.NodeGroup_Kind:
			nodes = true
		case awseks.ClusterAuth_Kind:
			auth = auth || !crossplaneaws.IsLiveIdentity(prefix, v.GetName())
		}
	}
	return eks && nodes && auth
}
