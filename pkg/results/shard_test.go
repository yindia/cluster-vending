package results_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/results"
	"github.com/yindia/cluster-vending/pkg/results/resultstest"
)

// A shard is its own latest and history objects under the kind's prefixes,
// so EmptyBucket finds them and a bad name cannot leave the prefix.
func TestPutShardWritesUnderTheKindsPrefixes(t *testing.T) {
	f := resultstest.New("b")
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

	key, etag, err := results.NewWriter(f).PutShard(t.Context(), "b", results.Recommendations, "payments", &cloudv1.ClusterDeprecations{}, at)

	require.NoError(t, err)
	require.Equal(t, "latest/recommendations/payments.json", key)
	require.NotEmpty(t, etag)
	require.Equal(t, []string{"history/recommendations/payments/2026-11-19T10:00:00Z.json", key}, f.Keys("b"))
	for _, bad := range []string{"", "../x", "A", "a/b", "-a"} {
		_, _, err := results.NewWriter(f).PutShard(t.Context(), "b", results.Recommendations, bad, &cloudv1.ClusterDeprecations{}, at)
		require.Error(t, err, bad)
	}
	n, err := results.EmptyBucket(t.Context(), f, "b")
	require.NoError(t, err)
	require.Equal(t, 2, n)
}
