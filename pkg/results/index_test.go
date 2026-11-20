package results_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/results/resultstest"
)

var idxAt = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func group(name string, n int) *cloudv1.NodeGroupNodes {
	g := &cloudv1.NodeGroupNodes{Group: name}
	for i := range n {
		g.Nodes = append(g.Nodes, &cloudv1.NodeGroupNode{Name: fmt.Sprintf("%s-ip-10-0-%d-%d.ec2.internal", name, i/250, i%250),
			InstanceType: "m7i.large", Zone: "us-east-1a", Ready: true, CreatedAt: timestamppb.New(idxAt)})
	}
	return g
}

func shardsOf(groups ...*cloudv1.NodeGroupNodes) map[string]proto.Message {
	out := map[string]proto.Message{}
	for _, g := range groups {
		out[g.Group] = g
	}
	return out
}

func TestIndexKeyIsUnderTheKindAndNoShardCanTakeIt(t *testing.T) {
	require.Equal(t, "latest/nodes/index.json", results.Nodes.IndexKey())
	require.False(t, results.ValidShard("index"), "a shard named index would overwrite the index")
	require.True(t, results.Nodes.Sharded())
	require.False(t, results.Deprecations.Sharded())
	require.True(t, results.Nodes.Valid())
}

func TestPutShardedWritesShardsThenTheIndex(t *testing.T) {
	f := resultstest.New("b")
	res, err := results.NewWriter(f).PutSharded(t.Context(), "b", results.Nodes,
		shardsOf(group("system", 2), group("batch", 3)), idxAt)

	require.NoError(t, err)
	require.Equal(t, results.Nodes.IndexKey(), res.Key)
	require.NotEmpty(t, res.ETag)
	require.False(t, res.Unchanged)
	require.Equal(t, []string{
		"history/nodes/batch/2026-11-19T10:00:00Z.json", "history/nodes/system/2026-11-19T10:00:00Z.json",
		"latest/nodes/batch.json", "latest/nodes/index.json", "latest/nodes/system.json"}, f.Keys("b"))
	// The index is written last, after every shard.
	require.Equal(t, "b/latest/nodes/index.json", f.Puts[len(f.Puts)-1])

	idx, etag, collected, err := results.NewReader(f).Index(t.Context(), "b", results.Nodes)
	require.NoError(t, err)
	require.Equal(t, res.ETag, etag)
	require.True(t, collected.Equal(idxAt))
	require.True(t, idx.GetComplete())
	require.Len(t, idx.GetShards(), 2)
	// Sorted by shard name; count is the shard's rows; key and ETag are the shard's.
	batch := idx.GetShards()[0]
	require.Equal(t, "batch", batch.GetShard())
	require.Equal(t, results.Nodes.ShardLatestKey("batch"), batch.GetKey())
	require.EqualValues(t, 3, batch.GetCount())
	require.NotEmpty(t, batch.GetDigest())
	_, shardETag, ok := f.Object("b", batch.GetKey())
	require.True(t, ok)
	require.Equal(t, shardETag, batch.GetEtag())
	require.EqualValues(t, 2, idx.GetShards()[1].GetCount())
}

func TestPutShardedWritesOnlyWhatChanged(t *testing.T) {
	f := resultstest.New("b")
	w := results.NewWriter(f)
	_, err := w.PutSharded(t.Context(), "b", results.Nodes, shardsOf(group("system", 2), group("batch", 3), group("gpu", 1)), idxAt)
	require.NoError(t, err)

	t.Run("unchanged content writes nothing", func(t *testing.T) {
		before := len(f.Puts)
		res, err := w.PutSharded(t.Context(), "b", results.Nodes, shardsOf(group("system", 2), group("batch", 3), group("gpu", 1)), idxAt.Add(time.Minute))
		require.NoError(t, err)
		require.True(t, res.Unchanged)
		require.Len(t, f.Puts, before)
	})
	t.Run("one changed shard rewrites it and the index only", func(t *testing.T) {
		before := len(f.Puts)
		res, err := w.PutSharded(t.Context(), "b", results.Nodes, shardsOf(group("system", 2), group("batch", 4), group("gpu", 1)), idxAt.Add(2*time.Minute))
		require.NoError(t, err)
		require.False(t, res.Unchanged)
		require.Equal(t, []string{"b/history/nodes/batch/2026-11-19T10:02:00Z.json", "b/latest/nodes/batch.json", "b/latest/nodes/index.json"}, f.Puts[before:])
		idx, _, _, err := results.NewReader(f).Index(t.Context(), "b", results.Nodes)
		require.NoError(t, err)
		require.EqualValues(t, 4, idx.GetShards()[0].GetCount())
	})
	t.Run("a removed shard is deleted and leaves the index", func(t *testing.T) {
		res, err := w.PutSharded(t.Context(), "b", results.Nodes, shardsOf(group("system", 2), group("batch", 4)), idxAt.Add(3*time.Minute))
		require.NoError(t, err)
		require.False(t, res.Unchanged)
		require.Equal(t, []string{"gpu"}, res.Removed)
		_, _, ok := f.Object("b", "latest/nodes/gpu.json")
		require.False(t, ok)
		idx, _, _, err := results.NewReader(f).Index(t.Context(), "b", results.Nodes)
		require.NoError(t, err)
		require.Len(t, idx.GetShards(), 2)
	})
	t.Run("completeness is part of the index", func(t *testing.T) {
		res, err := w.PutSharded(t.Context(), "b", results.Nodes, shardsOf(group("system", 2), group("batch", 4)), idxAt.Add(4*time.Minute), results.Incomplete())
		require.NoError(t, err)
		require.False(t, res.Unchanged)
		idx, _, _, err := results.NewReader(f).Index(t.Context(), "b", results.Nodes)
		require.NoError(t, err)
		require.False(t, idx.GetComplete())
	})
}

func TestPutShardedRefusesBadInputWithoutWriting(t *testing.T) {
	for name, shards := range map[string]map[string]proto.Message{
		"a shard named index": shardsOf(group("index", 1)),
		"an invalid name":     {"Bad/Name": group("x", 1)},
		"a nil shard":         {"system": nil},
	} {
		t.Run(name, func(t *testing.T) {
			f := resultstest.New("b")
			_, err := results.NewWriter(f).PutSharded(t.Context(), "b", results.Nodes, shards, idxAt)
			require.Error(t, err)
			require.Empty(t, f.Keys("b"))
		})
	}
	f := resultstest.New("b")
	_, err := results.NewWriter(f).PutSharded(t.Context(), "b", results.Deprecations, shardsOf(group("system", 1)), idxAt)
	require.Error(t, err, "a kind that is not sharded")
	_, err = results.NewWriter(f).PutSharded(t.Context(), "", results.Nodes, shardsOf(group("system", 1)), idxAt)
	require.Error(t, err)
}

func TestPutShardedAnEmptySetWritesAnEmptyIndex(t *testing.T) {
	f := resultstest.New("b")
	w := results.NewWriter(f)
	_, err := w.PutSharded(t.Context(), "b", results.Nodes, shardsOf(group("system", 1)), idxAt)
	require.NoError(t, err)
	res, err := w.PutSharded(t.Context(), "b", results.Nodes, nil, idxAt.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, []string{"system"}, res.Removed)
	idx, _, _, err := results.NewReader(f).Index(t.Context(), "b", results.Nodes)
	require.NoError(t, err)
	require.Empty(t, idx.GetShards())
}

func TestPutShardedStartsOverWhenTheIndexIsUnreadable(t *testing.T) {
	f := resultstest.New("b")
	f.Set("b", results.Nodes.IndexKey(), []byte("not gzip"))
	res, err := results.NewWriter(f).PutSharded(t.Context(), "b", results.Nodes, shardsOf(group("system", 1)), idxAt)
	require.NoError(t, err)
	require.False(t, res.Unchanged)
	_, _, _, err = results.NewReader(f).Index(t.Context(), "b", results.Nodes)
	require.NoError(t, err)
}

func TestPutShardedFailureLeavesTheOldIndex(t *testing.T) {
	f := resultstest.New("b")
	w := results.NewWriter(f)
	_, err := w.PutSharded(t.Context(), "b", results.Nodes, shardsOf(group("system", 1)), idxAt)
	require.NoError(t, err)
	_, oldETag, _ := f.Object("b", results.Nodes.IndexKey())
	f.Fail = func(op, _, key string) error {
		if op == "PutObject" && key == "latest/nodes/system.json" {
			return fmt.Errorf("boom")
		}
		return nil
	}
	_, err = w.PutSharded(t.Context(), "b", results.Nodes, shardsOf(group("system", 2)), idxAt.Add(time.Minute))
	require.Error(t, err)
	_, etag, _ := f.Object("b", results.Nodes.IndexKey())
	require.Equal(t, oldETag, etag)
}

func TestReaderIndexAndLatestShardUseTheETagCache(t *testing.T) {
	f := resultstest.New("b")
	// 1,000 nodes in one group and 10 groups in all.
	set := []*cloudv1.NodeGroupNodes{group("system", 2), group("big", 1000)}
	for i := range 8 {
		set = append(set, group(fmt.Sprintf("g%d", i), 5))
	}
	_, err := results.NewWriter(f).PutSharded(t.Context(), "b", results.Nodes, shardsOf(set...), idxAt)
	require.NoError(t, err)

	r := results.NewReader(f)
	gets := f.Calls["GetObject"]
	idx, _, _, err := r.Index(t.Context(), "b", results.Nodes)
	require.NoError(t, err)
	require.Len(t, idx.GetShards(), 10)

	into := &cloudv1.NodeGroupNodes{}
	etag, collected, err := r.LatestShard(t.Context(), "b", results.Nodes, "big", into)
	require.NoError(t, err)
	require.Len(t, into.GetNodes(), 1000)
	require.True(t, collected.Equal(idxAt))
	require.NotEmpty(t, etag)

	again := &cloudv1.NodeGroupNodes{}
	etag2, _, err := r.LatestShard(t.Context(), "b", results.Nodes, "big", again)
	require.NoError(t, err)
	require.Equal(t, etag, etag2)
	require.Len(t, again.GetNodes(), 1000)
	// Only the index and the shards read are cached.
	require.Equal(t, 2, r.CachedEntries())
	// A page of a 1,000-node group is one GET of its shard plus one of the index.
	require.Equal(t, 3, f.Calls["GetObject"]-gets, "the index, the shard, and the shard revalidated")
}

func TestReaderLatestShardErrors(t *testing.T) {
	f := resultstest.New("b")
	r := results.NewReader(f)
	_, _, _, err := r.Index(t.Context(), "b", results.Nodes)
	require.ErrorIs(t, err, results.ErrNotFound)
	_, _, err = r.LatestShard(t.Context(), "b", results.Nodes, "system", &cloudv1.NodeGroupNodes{})
	require.ErrorIs(t, err, results.ErrNotFound)
	_, _, err = r.LatestShard(t.Context(), "b", results.Nodes, "../x", &cloudv1.NodeGroupNodes{})
	require.Error(t, err)
	_, _, err = r.LatestShard(t.Context(), "", results.Nodes, "system", &cloudv1.NodeGroupNodes{})
	require.Error(t, err)
}

func TestPutWithoutHistoryWritesOnlyLatest(t *testing.T) {
	f := resultstest.New("b")
	w := results.NewWriter(f)

	key, etag, err := w.Put(t.Context(), "b", results.Recommendations, &cloudv1.ClusterDeprecations{}, idxAt, results.WithoutHistory())

	require.NoError(t, err)
	require.Equal(t, "latest/recommendations.json", key)
	require.NotEmpty(t, etag)
	require.Equal(t, []string{key}, f.Keys("b"))
	// Without the option, as before: history, then latest.
	_, _, err = w.Put(t.Context(), "b", results.Violations, &cloudv1.ClusterDeprecations{}, idxAt)
	require.NoError(t, err)
	require.Contains(t, f.Keys("b"), "history/violations/2026-11-19T10:00:00Z.json")
}
