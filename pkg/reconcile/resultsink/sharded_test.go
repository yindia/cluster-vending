package resultsink_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/results"
)

func groupNodes(name string, n int) *cloudv1.NodeGroupNodes {
	g := &cloudv1.NodeGroupNodes{Group: name}
	for i := range n {
		g.Nodes = append(g.Nodes, &cloudv1.NodeGroupNode{Name: fmt.Sprintf("%s-%03d", name, i), Ready: true})
	}
	return g
}

func shards(groups ...*cloudv1.NodeGroupNodes) map[string]proto.Message {
	out := map[string]proto.Message{}
	for _, g := range groups {
		out[g.Group] = g
	}
	return out
}

func TestStageShardedWritesShardsAndTheIndexAndCommitRecordsTheIndex(t *testing.T) {
	w := newWorld(t, false)
	w.bucketObject(t, true)

	staged, notice := w.sink.StageSharded(t.Context(), tenant, clusterID, results.Nodes, shards(groupNodes("system", 2), groupNodes("batch", 3)), now,
		model.ResultRef{Total: 5, CollectedAt: now})

	require.Empty(t, notice)
	require.NotNil(t, staged)
	require.False(t, staged.Unchanged)
	require.Equal(t, []string{"history/nodes/batch/2026-11-19T10:00:00Z.json", "history/nodes/system/2026-11-19T10:00:00Z.json",
		"latest/nodes/batch.json", "latest/nodes/index.json", "latest/nodes/system.json"}, w.s3.Keys(w.bucket))
	require.NoError(t, staged.Commit(t.Context()))
	ref := w.refOf(t, results.Nodes)
	require.NotNil(t, ref)
	_, etag, _ := w.s3.Object(w.bucket, results.Nodes.IndexKey())
	require.Equal(t, results.Nodes.IndexKey(), ref.Key)
	require.Equal(t, etag, ref.ETag)
	require.Equal(t, w.bucket, ref.Bucket)
	require.EqualValues(t, 5, ref.Total)
	require.NotEmpty(t, ref.Digest)
}

func TestStageShardedWritesNothingForTheSameContent(t *testing.T) {
	w := newWorld(t, false)
	w.bucketObject(t, true)
	set := func() map[string]proto.Message { return shards(groupNodes("system", 2), groupNodes("batch", 3)) }
	staged, _ := w.sink.StageSharded(t.Context(), tenant, clusterID, results.Nodes, set(), now, model.ResultRef{Total: 5})
	require.NoError(t, staged.Commit(t.Context()))
	puts := len(w.s3.Puts)

	again, notice := w.sink.StageSharded(t.Context(), tenant, clusterID, results.Nodes, set(), now.Add(time.Minute), model.ResultRef{Total: 5})

	require.Empty(t, notice)
	require.NotNil(t, again)
	require.True(t, again.Unchanged)
	require.Len(t, w.s3.Puts, puts, "nothing is written")
	require.Equal(t, results.Nodes.IndexKey(), again.Ref().Key)
	require.True(t, again.Ref().CollectedAt.Equal(now.Add(time.Minute)), "the summary moves its time")

	// One more node: written again, and a different digest.
	changed := set()
	changed["batch"] = groupNodes("batch", 4)
	third, _ := w.sink.StageSharded(t.Context(), tenant, clusterID, results.Nodes, changed, now.Add(2*time.Minute), model.ResultRef{Total: 6})
	require.False(t, third.Unchanged)
	require.NotEqual(t, again.Ref().Digest, third.Ref().Digest)
	require.Greater(t, len(w.s3.Puts), puts)
}

func TestStageShardedIncompleteChangesTheDigest(t *testing.T) {
	w := newWorld(t, false)
	w.bucketObject(t, true)
	a, _ := w.sink.StageSharded(t.Context(), tenant, clusterID, results.Nodes, shards(groupNodes("system", 2)), now, model.ResultRef{})
	require.NoError(t, a.Commit(t.Context()))
	b, _ := w.sink.StageSharded(t.Context(), tenant, clusterID, results.Nodes, shards(groupNodes("system", 2)), now, model.ResultRef{Incomplete: true}, results.Incomplete())
	require.False(t, b.Unchanged)
	require.NotEqual(t, a.Ref().Digest, b.Ref().Digest)
}

// The notice of a node list says what the node list is, not the Cluster
// object's (which never holds it).
func TestStageShardedNotices(t *testing.T) {
	for name, tt := range map[string]struct {
		setup  func(*world, *testing.T)
		notice string
	}{
		"no bucket yet": {func(*world, *testing.T) {}, "the node list is unavailable: the results bucket is not created yet"},
		"not ready":     {func(w *world, t *testing.T) { w.bucketObject(t, false) }, "the node list is unavailable: the results bucket is not ready yet"},
		"no access": {func(w *world, t *testing.T) {
			w.bucketObject(t, true)
			w.deps.ResultsS3 = func(awscloud.Access, string) (results.S3API, error) { return nil, errors.New("down") }
			w.sink = newSink(w)
		}, "the node list is unavailable: the cluster's account could not be reached"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, false)
			tt.setup(w, t)
			staged, notice := w.sink.StageSharded(t.Context(), tenant, clusterID, results.Nodes, shards(groupNodes("system", 1)), now, model.ResultRef{})
			require.Nil(t, staged)
			require.Equal(t, tt.notice, notice)
		})
	}
	t.Run("an imported cluster without a bucket is silent", func(t *testing.T) {
		w := newWorld(t, true)
		staged, notice := w.sink.StageSharded(t.Context(), tenant, clusterID, results.Nodes, shards(groupNodes("system", 1)), now, model.ResultRef{})
		require.Nil(t, staged)
		require.Empty(t, notice)
	})
	t.Run("a write failure", func(t *testing.T) {
		w := newWorld(t, false)
		w.bucketObject(t, true)
		w.s3.Fail = func(op, _, _ string) error {
			if op == "PutObject" {
				return errors.New("boom")
			}
			return nil
		}
		staged, notice := w.sink.StageSharded(t.Context(), tenant, clusterID, results.Nodes, shards(groupNodes("system", 1)), now, model.ResultRef{})
		require.Nil(t, staged)
		require.Equal(t, "the node list is unavailable: writing to the results bucket failed", notice)
	})
}

func TestStageShardedNeedsAShardedKind(t *testing.T) {
	w := newWorld(t, false)
	w.bucketObject(t, true)
	staged, notice := w.sink.StageSharded(t.Context(), tenant, clusterID, results.Deprecations, shards(groupNodes("system", 1)), now, model.ResultRef{})
	require.Nil(t, staged)
	require.NotEmpty(t, notice)
}
