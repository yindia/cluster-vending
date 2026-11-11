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

// ClusterDelete calls ClusterService.DeleteCluster. The server deletes the
// cluster's resources in order (add-ons, node groups, the EKS cluster, IAM),
// one step per read; --wait reads until the cluster is gone.
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

// waitCluster waits until the cluster is RUNNING (gone: deleted) and
// returns its last read.
func waitCluster(ctx context.Context, c Clients, o Options, tenant, id string, gone bool) (proto.Message, error) {
	var last proto.Message
	err := waitUntil(ctx, o, "cluster "+id, gone, func(ctx context.Context) (observation, error) {
		res, err := call(ctx, &cloudv1.GetClusterRequest{TenantId: tenant, ClusterId: id}, c.Cluster.GetCluster)
		if err != nil {
			return observation{}, err
		}
		last = res
		st := res.(*cloudv1.GetClusterResponse).GetCluster().GetStatus()
		state := st.GetState()
		return observation{
			state: state.String(), message: st.GetMessage(),
			done:   !gone && state == cloudv1.ClusterState_CLUSTER_STATE_RUNNING,
			failed: !gone && state == cloudv1.ClusterState_CLUSTER_STATE_FAILED,
		}, nil
	})
	return last, err
}
