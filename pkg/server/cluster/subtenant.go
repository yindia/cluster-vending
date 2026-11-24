package cluster

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/naas"
)

// requireNoSubTenants reads through the direct API reader, not the informer
// cache. List the entire namespace so a missing or stale label cannot hide a
// child whose spec still references this cluster. Terminating children keep
// blocking NaaS disabling until Kubernetes has actually removed them; a
// deletion is refused only for live ones (the reconciler holds it until the
// terminating ones are gone).
func (s *ClusterService) requireNoSubTenants(ctx context.Context, row *model.Cluster, deleting bool) error {
	dependents := naas.ClusterDependents
	if deleting {
		dependents = naas.LiveClusterDependents
	}
	names, err := dependents(ctx, s.deps.Reader(), naas.ClusterIdentity{
		Namespace: kube.TenantNamespace(row.TenantID), ID: row.ID, Name: row.Name,
	})
	if err != nil {
		// Missing API registration/CRD and permission errors are not evidence
		// that there are no dependents. Fail closed and preserve the cause.
		return fmt.Errorf("list SubTenants before changing cluster %s: %w", row.ID, err)
	}
	if len(names) > 0 {
		return connect.NewError(connect.CodeFailedPrecondition, naas.ParentBlockedMessage("Cluster deletion or NaaS disabling", names))
	}
	return nil
}
