package cli

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/updatemask"
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
	tenant, err := m.tenantFor(o)
	if err != nil {
		return nil, err
	}
	cl, err := m.Cluster()
	if err != nil {
		return nil, err
	}
	addSelfAdmin(ctx, o, cl)
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
	return call(ctx, &cloudv1.GetClusterRequest{TenantId: o.Tenant, ClusterId: o.ID, Live: o.Live}, c.Cluster.GetCluster)
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
// manifest; the cluster is the argument or metadata.id. The mask is what
// the manifest changes: it reads the cluster, prints each change and sends
// the cluster's etag with them, so a change made meanwhile is refused
// (inferredMask). With --wait it
// returns once the cluster is RUNNING again.
func ClusterUpdate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadManifest(o.Input, KindCluster)
	if err != nil {
		return nil, err
	}
	tenant, err := m.tenantFor(o)
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
	got, err := call(ctx, &cloudv1.GetClusterRequest{TenantId: tenant, ClusterId: cl.GetId(), AsRequested: true}, c.Cluster.GetCluster)
	if err != nil {
		return nil, err
	}
	read := got.(*cloudv1.GetClusterResponse)
	current, desired := read.GetCluster(), cl
	if !read.GetAsRequested() {
		// Only the resolved spec: compare with what the server merges in.
		desired = clusterAsStored(cl)
	}
	desired = withoutAutomaticVersion(o, current, desired)
	mask, err := inferredMask(o, "cluster "+current.GetName(), current, desired, updatemask.Cluster)
	if err != nil || len(mask) == 0 {
		return nil, err
	}
	cl.Etag = current.GetEtag()

	req := &cloudv1.UpdateClusterRequest{TenantId: tenant, Cluster: cl, UpdateMask: &fieldmaskpb.FieldMask{Paths: mask}, ValidateOnly: true}
	preview, err := call(ctx, req, c.Cluster.UpdateCluster)
	if err != nil {
		return nil, err
	}
	shown := preview.(*cloudv1.UpdateClusterResponse)
	if o.Plan {
		return preview, nil
	}
	if err := reviewUpdatePlan(o, shown.GetPlan()); err != nil {
		return nil, err
	}
	req.ValidateOnly, req.PlanId = false, shown.GetPlan().GetPlanId()
	res, err := call(ctx, req, c.Cluster.UpdateCluster)
	if err != nil || !o.Wait {
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
// manifest. The call only stores the import: the answer is the cluster as
// IMPORTED with its import DISCOVERING, before the EKS cluster was looked
// at. With --wait it returns once the controller has looked the cluster
// up and the plan (status.import) is there: PLANNED with the resources
// observed, or BLOCKED. A cluster that does not exist or cannot be read
// ends the wait as an error (the cluster is FAILED, with the reason).
func ClusterImport(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadManifest(o.Input, KindClusterImport)
	if err != nil {
		return nil, err
	}
	tenant, err := m.tenantFor(o)
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
	res, err := call(ctx, req, c.Cluster.ImportCluster)
	if err != nil || !o.Wait {
		return res, err
	}
	id := res.(*cloudv1.ImportClusterResponse).GetCluster().GetId()
	var last proto.Message
	err = waitUntil(ctx, o, "cluster "+id, false, func(ctx context.Context) (observation, error) {
		res, err := call(ctx, &cloudv1.GetClusterRequest{TenantId: tenant, ClusterId: id, Live: true}, c.Cluster.GetCluster)
		if err != nil {
			return observation{}, err
		}
		last = res
		st := res.(*cloudv1.GetClusterResponse).GetCluster().GetStatus()
		return importObservation(st.GetState().String(), st.GetMessage(), st.GetImport(), len(st.GetManagedResources()),
			st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_FAILED), nil
	})
	return last, err
}

// ClusterTakeOver calls ClusterService.TakeOverCluster. With --wait it
// returns once the cluster is RUNNING under the platform.
func ClusterTakeOver(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.TakeOverClusterRequest{
		TenantId: o.Tenant, ClusterId: o.ID, Etag: o.ETag, AcknowledgeNotImported: o.AcknowledgeNotImported,
		SkipPlatformComponents: o.SkipPlatformComponents,
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
		// The live status: without live, Get returns what was last recorded.
		res, err := call(ctx, &cloudv1.GetClusterRequest{TenantId: tenant, ClusterId: id, Live: true}, c.Cluster.GetCluster)
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
