package cli

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ClusterCreate calls ClusterService.CreateCluster with the Cluster
// manifest; the server assigns the id. The request ID is derived from the
// request, so running the same create twice returns the first cluster.
// With --wait it returns once the cluster is RUNNING.
func ClusterCreate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadManifest(o.Input, KindCluster)
	if err != nil {
		return nil, err
	}
	if m.Metadata.ID != "" {
		return nil, fmt.Errorf("metadata.id %q: a cluster's id is assigned by the server", m.Metadata.ID)
	}
	tenant, err := m.tenant(o.Tenant)
	if err != nil {
		return nil, err
	}
	cl, err := m.Cluster()
	if err != nil {
		return nil, err
	}
	req := &cloudv1.CreateClusterRequest{TenantId: tenant, Cluster: cl}
	if req.RequestId, err = requestID(tenant, req); err != nil {
		return nil, err
	}
	res, err := call(ctx, req, c.Cluster.CreateCluster)
	if err != nil || !o.Wait {
		return res, err
	}
	return waitCluster(ctx, c, o, tenant, res.(*cloudv1.CreateClusterResponse).GetCluster().GetId(), false)
}

// ClusterGet calls ClusterService.GetCluster.
func ClusterGet(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.GetClusterRequest{TenantId: o.Tenant, ClusterId: o.ID}, c.Cluster.GetCluster)
}

// ClusterStatus calls ClusterService.GetClusterStatus: the cluster with
// its status read live from the management cluster.
func ClusterStatus(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.GetClusterStatusRequest{TenantId: o.Tenant, ClusterId: o.ID}, c.Cluster.GetClusterStatus)
}

// ClusterList calls ClusterService.ListClusters.
func ClusterList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.ListClustersRequest{
		TenantId: o.Tenant, PageSize: o.PageSize, PageToken: o.PageToken,
	}, c.Cluster.ListClusters)
}

// ClusterUpdate calls ClusterService.UpdateCluster with the Cluster
// manifest and --update-mask; the cluster is the argument or metadata.id.
// With --wait it returns once the cluster is RUNNING again.
func ClusterUpdate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadManifest(o.Input, KindCluster)
	if err != nil {
		return nil, err
	}
	tenant, err := m.tenant(o.Tenant)
	if err != nil {
		return nil, err
	}
	if m.Metadata.ID, err = updateID(o.ID, m.Metadata.ID); err != nil {
		return nil, err
	}
	cl, err := m.Cluster()
	if err != nil {
		return nil, err
	}
	if o.ETag != "" {
		cl.Etag = o.ETag
	}
	res, err := call(ctx, &cloudv1.UpdateClusterRequest{
		TenantId: tenant, Cluster: cl, UpdateMask: &fieldmaskpb.FieldMask{Paths: o.UpdateMask},
		ValidateOnly: o.Plan, PlanId: o.PlanID,
	}, c.Cluster.UpdateCluster)
	if err != nil || !o.Wait || o.Plan {
		return res, err
	}
	return waitCluster(ctx, c, o, tenant, cl.GetId(), false)
}

// ClusterDelete calls ClusterService.DeleteCluster, which stores the
// deletion (the answer is the cluster, DELETING). The server then deletes
// the cluster's resources in order (add-ons, node groups, the EKS cluster,
// IAM); --wait reads until the cluster is gone.
func ClusterDelete(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.DeleteClusterRequest{TenantId: o.Tenant, ClusterId: o.ID, Etag: o.ETag, Force: o.Force},
		c.Cluster.DeleteCluster)
	if err != nil || !o.Wait {
		return res, err
	}
	if _, err := waitCluster(ctx, c, o, o.Tenant, o.ID, true); err != nil {
		return nil, err
	}
	return res, nil
}

// ClusterImport calls ClusterService.ImportCluster with the ClusterImport
// manifest. It returns the imported cluster with its plan (status.import).
func ClusterImport(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadManifest(o.Input, KindClusterImport)
	if err != nil {
		return nil, err
	}
	tenant, err := m.tenant(o.Tenant)
	if err != nil {
		return nil, err
	}
	req, err := m.ClusterImport()
	if err != nil {
		return nil, err
	}
	req.TenantId = tenant
	if req.GetRequestId() == "" {
		if req.RequestId, err = requestID(tenant, req); err != nil {
			return nil, err
		}
	}
	return call(ctx, req, c.Cluster.ImportCluster)
}

// ClusterTakeOver calls ClusterService.TakeOverCluster. With --wait it
// returns once the cluster is RUNNING under the platform.
func ClusterTakeOver(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.TakeOverClusterRequest{
		TenantId: o.Tenant, ClusterId: o.ID, Etag: o.ETag, AcknowledgeNotImported: o.AcknowledgeNotImported,
	}, c.Cluster.TakeOverCluster)
	if err != nil || !o.Wait {
		return res, err
	}
	return waitCluster(ctx, c, o, o.Tenant, o.ID, false)
}

// waitCluster waits until the cluster is RUNNING (gone: deleted) and
// returns its last read. A call only stores what was asked for; what goes
// wrong applying it is the cluster's status: FAILED ends the wait with its
// message as the error.
func waitCluster(ctx context.Context, c Clients, o Options, tenant, id string, gone bool) (proto.Message, error) {
	var last proto.Message
	err := waitUntil(ctx, o, "cluster "+id, gone, func(ctx context.Context) (observation, error) {
		// The live status: Get returns what the server last recorded.
		res, err := call(ctx, &cloudv1.GetClusterStatusRequest{TenantId: tenant, ClusterId: id}, c.Cluster.GetClusterStatus)
		if err != nil {
			return observation{}, err
		}
		last = res
		st := res.(*cloudv1.GetClusterStatusResponse).GetCluster().GetStatus()
		state := st.GetState()
		return observation{
			state: state.String(), message: st.GetMessage(),
			done:   !gone && state == cloudv1.ClusterState_CLUSTER_STATE_RUNNING,
			failed: !gone && state == cloudv1.ClusterState_CLUSTER_STATE_FAILED,
		}, nil
	})
	return last, err
}
