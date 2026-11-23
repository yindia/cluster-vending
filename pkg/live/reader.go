package live

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// kindNodes is the build of nodes, pods, NodeClaims and node metrics: one
// build serves the node rows of every group and the totals.
const kindNodes = "nodes"

// Opener resolves the Lister of a cluster from its tenant and cluster IDs
// only (7.2): it never takes a kubeconfig, a server URL or a Secret name from
// a request. It fails with a live Error (NO_LIVE_IDENTITY, or
// CLUSTER_UNREACHABLE "no cluster").
type Opener interface {
	Open(ctx context.Context, tenantID, clusterID string) (Lister, error)
}

// Reader serves the live reads: it builds a cluster's view on request,
// keeps it briefly and pages through it.
type Reader struct {
	open  Opener
	lim   Limits
	now   func() time.Time
	store *store
	guard *guard
	log   *slog.Logger
}

// New is a Reader with the limits lim.
func New(open Opener, lim Limits, log *slog.Logger) *Reader {
	if log == nil {
		log = slog.Default()
	}
	return &Reader{open: open, lim: lim, now: time.Now, store: newStore(lim), guard: newGuard(lim), log: log}
}

// nodes is the cluster's nodes build: the one of the last Reuse window, or a
// new one, built once however many ask at the same time.
func (r *Reader) nodes(ctx context.Context, tenantID, clusterID string) (*cachedBuild, error) {
	key := buildKey{tenant: tenantID, cluster: clusterID, kind: kindNodes}
	now := r.now()
	r.store.sweep(now)
	if cb := r.store.fresh(key, now); cb != nil {
		return cb, nil
	}
	return r.guard.do(ctx, key, func() (*cachedBuild, error) {
		now := r.now()
		if cb := r.store.fresh(key, now); cb != nil {
			return cb, nil
		}
		if err := r.guard.cooling(key, now); err != nil {
			refusedTotal.WithLabelValues("cooldown").Inc()
			return nil, err
		}
		if err := r.guard.admit(key, now); err != nil {
			refusedTotal.WithLabelValues("busy").Inc()
			return nil, err
		}
		release, err := r.guard.slot(ctx)
		if err != nil {
			refusedTotal.WithLabelValues("busy").Inc()
			return nil, err
		}
		defer release()
		b, berr := r.build(ctx, key, now)
		var le *Error
		switch {
		case berr == nil:
			r.guard.succeeded(key)
			cb := r.store.put(key, b, now)
			viewBytes.Set(float64(r.store.size()))
			return cb, nil
		case errors.As(berr, &le):
			r.guard.failed(key, le, now)
		}
		return nil, berr
	})
}

// build opens the cluster and reads it within the deadline. The caller's
// cancellation does not stop it (others may wait on it); the deadline does.
func (r *Reader) build(ctx context.Context, key buildKey, now time.Time) (*nodeBuild, error) {
	start := time.Now()
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.lim.NodesDeadline)
	defer cancel()
	var b *nodeBuild
	l, err := r.open.Open(bctx, key.tenant, key.cluster)
	if err == nil {
		b, err = buildNodes(bctx, l, r.lim, now)
	}
	attrs := []any{"tenant_id", key.tenant, "cluster_id", key.cluster, "kind", key.kind, "duration", time.Since(start)}
	buildSeconds.WithLabelValues(key.kind).Observe(time.Since(start).Seconds())
	if err != nil {
		reason := "error"
		var le *Error
		if errors.As(err, &le) {
			reason = le.Reason
		}
		buildsTotal.WithLabelValues(key.kind, reason).Inc()
		r.log.InfoContext(ctx, "live build", append(attrs, "outcome", "failed", "reason", reason)...)
		return nil, err
	}
	buildsTotal.WithLabelValues(key.kind, "ok").Inc()
	r.log.InfoContext(ctx, "live build", append(attrs, "outcome", "ok", "rows", len(b.rows), "pages", b.pages, "bytes", b.bytes)...)
	return b, nil
}

// ListNodeGroupNodes is a page of one node group's nodes, read live. A page
// token names the view it came from: one that is gone or that another
// request made answers INVALID_ARGUMENT "restart the list".
func (r *Reader) ListNodeGroupNodes(ctx context.Context, req *cloudv1.ListNodeGroupNodesRequest) (*cloudv1.ListNodeGroupNodesResponse, error) {
	sc := viewScope{tenant: req.GetTenantId(), cluster: req.GetClusterId(), group: req.GetNodeGroup(), nameContains: req.GetNameContains(),
		filter: req.GetFilter(), sortBy: req.GetSortBy(), desc: req.GetSortOrder() == cloudv1.SortOrder_SORT_ORDER_DESC}
	if sc.filter == cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_ALL {
		sc.filter = cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_UNSPECIFIED
	}
	if sc.sortBy == cloudv1.NodeSortBy_NODE_SORT_BY_UNSPECIFIED {
		sc.sortBy = cloudv1.NodeSortBy_NODE_SORT_BY_NAME
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = r.lim.DefaultPageSize
	}
	size = min(size, r.lim.MaxPageSize)
	if token := req.GetPageToken(); token != "" {
		id, offset, ok := parseToken(token)
		if !ok {
			return nil, errRestart()
		}
		v, ok := r.store.view(id, r.now())
		if !ok || v.scope != sc || offset > len(v.rows) {
			return nil, errRestart()
		}
		return r.page(v.parent, v.id, v.rows, offset, size), nil
	}
	cb, err := r.nodes(ctx, sc.tenant, sc.cluster)
	if err != nil {
		return nil, err
	}
	rows := selectNodes(cb.build, sc)
	id := ""
	if len(rows) > size {
		id = r.store.newView(cb, sc, rows, r.now())
	}
	return r.page(cb, id, rows, 0, size), nil
}

func (r *Reader) page(cb *cachedBuild, id string, rows []*cloudv1.NodeGroupNode, offset, size int) *cloudv1.ListNodeGroupNodesResponse {
	end := min(offset+size, len(rows))
	out := &cloudv1.ListNodeGroupNodesResponse{Nodes: rows[offset:end], Total: int32(len(rows)), Complete: true, //nolint:gosec // bounded by MaxNodeRows
		Notices: cb.build.notices, LiveAt: timestamppb.New(cb.build.liveAt)}
	if end < len(rows) && id != "" {
		out.NextPageToken = viewToken(id, end)
	}
	return out
}

// GetNodeResources is the totals per group and for the cluster, from the same
// build as the node rows.
func (r *Reader) GetNodeResources(ctx context.Context, req *cloudv1.GetNodeResourcesRequest) (*cloudv1.GetNodeResourcesResponse, error) {
	cb, err := r.nodes(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	b := cb.build
	return &cloudv1.GetNodeResourcesResponse{Groups: b.groups, Cluster: b.cluster, LiveAt: timestamppb.New(b.liveAt),
		Complete: true, Notices: b.notices}, nil
}
