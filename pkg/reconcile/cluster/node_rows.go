package cluster

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile/resultsink"
	"github.com/yindia/cluster-vending/pkg/results"
)

// The per-node rows of the node groups (the `nodes` kind of the cluster's
// results bucket, docs/architecture.md#results-in-s3): collected during the
// reads readNodes already makes, one shard per node group, written at most
// once a minute per cluster and only when their content changed. The Cluster
// keeps only counts.
const (
	// systemNodeGroup is the group of the default managed node group's nodes.
	systemNodeGroup = "system"
	// managedNodeGroupLabel is the label EKS puts on the nodes of a managed
	// node group.
	managedNodeGroupLabel = "eks.amazonaws.com/nodegroup"
	// nodeRowsInterval is how often a cluster's rows may be written.
	nodeRowsInterval = time.Minute
	// nodeRowsRefresh is how old the summary's collected time gets before an
	// unchanged collection moves it.
	nodeRowsRefresh = 10 * time.Minute
	// nodeRowsWriteTimeout bounds the write, whatever is left of the pass.
	nodeRowsWriteTimeout = 20 * time.Second
)

// nodeRowWrites remembers when each cluster's rows were last written (or
// tried), so a pass every few seconds writes once a minute.
type nodeRowWrites struct {
	mu sync.Mutex
	// every is how often a cluster is due; zero is nodeRowsInterval.
	every time.Duration
	last  map[string]time.Time
}

// claim reports whether the cluster's rows are due at at, and if so records
// the attempt.
func (w *nodeRowWrites) claim(id string, at time.Time) bool {
	if w == nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	every := w.every
	if every == 0 {
		every = nodeRowsInterval
	}
	if last, ok := w.last[id]; ok && at.Sub(last) < every {
		return false
	}
	if w.last == nil {
		w.last = map[string]time.Time{}
	}
	w.last[id] = at
	return true
}

// forget drops the cluster's entry: a deleted cluster is not written again,
// and the map would keep one entry for every cluster that ever was.
func (w *nodeRowWrites) forget(id string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.last, id)
}

// shardsOf are the node rows as shards: one per group whose name can be a
// shard, its rows sorted by name; and how many rows there are.
func shardsOf(rows map[string][]*cloudv1.NodeGroupNode) (map[string]proto.Message, int32) {
	out := map[string]proto.Message{}
	total := 0
	for group, nodes := range rows {
		if !results.ValidShard(group) || len(nodes) == 0 {
			continue
		}
		sorted := slices.Clone(nodes)
		slices.SortFunc(sorted, func(a, b *cloudv1.NodeGroupNode) int { return strings.Compare(a.GetName(), b.GetName()) })
		out[group] = &cloudv1.NodeGroupNodes{Group: group, Nodes: sorted}
		total += len(sorted)
	}
	return out, int32(total)
}

// writeNodeRows writes the pass's node rows to the cluster's results bucket
// and records the summary. attempted is false when it did nothing (throttled,
// no S3, or nothing was read); otherwise notice is why the node list could not
// be stored, "" when it was (or the cluster has no bucket by design). It never
// fails the pass.
func (s *Reconciler) writeNodeRows(ctx context.Context, row *model.Cluster, nodes nodeCounts) (attempted bool, notice string) {
	if s.deps.ResultsS3 == nil || s.deps.Store == nil {
		return false, ""
	}
	shards, total := shardsOf(nodes.rows)
	if len(shards) == 0 && !nodes.complete {
		return false, "" // nothing could be read: the list stored last stays
	}
	at := now()
	if !s.nodeRows.claim(row.ID, at) {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nodeRowsWriteTimeout)
	defer cancel()
	var opts []results.ShardOption
	if !nodes.complete {
		opts = append(opts, results.Incomplete())
	}
	summary := model.ResultRef{Total: total, CollectedAt: at, AttemptedAt: at, Incomplete: !nodes.complete}
	staged, notice := resultsink.New(s.deps, s.logger()).StageSharded(ctx, row.TenantID, row.ID, results.Nodes, shards, at, summary, opts...)
	if staged == nil {
		return true, notice
	}
	// An unchanged list moves the summary's time only now and then.
	if cur := row.ResultRef(string(results.Nodes)); !staged.Unchanged || cur == nil || at.Sub(cur.CollectedAt) >= nodeRowsRefresh {
		if err := staged.Commit(ctx); err != nil {
			s.logger().WarnContext(ctx, "node groups: record the node list summary", "cluster_id", row.ID, "error", err)
		}
	}
	return true, ""
}

// withNodesNotice sets the groups' node list notice: what this pass's attempt
// found, or, when it made none, what the last recorded status said.
func withNodesNotice(groups, stored []*cloudv1.NodeGroupStatus, attempted bool, notice string) {
	for _, g := range groups {
		g.NodesNotice = notice
		if attempted {
			continue
		}
		for _, o := range stored {
			if o.GetName() == g.GetName() {
				g.NodesNotice = o.GetNodesNotice()
			}
		}
	}
}
