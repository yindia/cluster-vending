package results

import (
	"regexp"
	"slices"
	"time"
)

// Kind is a kind of result: it names the object under latest/ and the
// history prefix.
type Kind string

// The kinds. Their message (the contract of the cloud.v1 API) is the
// caller's: Deprecations a cloudv1.ClusterDeprecations with its findings,
// Recommendations a cloudv1.ListClusterRecommendationsResponse (the
// recommendations with their summary), Violations and ViolationEvents a
// cloudv1.ClusterPolicySnapshot (its violations; its events).
const (
	Deprecations    Kind = "deprecations"
	Recommendations Kind = "recommendations"
	Violations      Kind = "violations"
	ViolationEvents Kind = "violation-events"
	// Nodes is a sharded kind: one cloudv1.NodeGroupNodes per node group
	// (latest/nodes/<group>.json) and a cloudv1.ResultIndex naming them
	// (latest/nodes/index.json).
	Nodes Kind = "nodes"
)

// Key prefixes of the objects.
const (
	LatestPrefix  = "latest/"
	HistoryPrefix = "history/"
)

var kinds = []Kind{Deprecations, Recommendations, Violations, ViolationEvents, Nodes}

// indexShard is the name the index takes under a sharded kind's prefix, so no
// shard may have it.
const indexShard = "index"

// Sharded reports whether the kind is stored as shards and an index (Nodes)
// and not as one object.
func (k Kind) Sharded() bool { return k == Nodes }

// IndexKey is the key of the index of a sharded kind: latest/<kind>/index.json.
func (k Kind) IndexKey() string { return LatestPrefix + string(k) + "/" + indexShard + ".json" }

// Kinds are all the kinds.
func Kinds() []Kind { return slices.Clone(kinds) }

// Valid reports whether k is one of the kinds (so never builds a key that
// escapes its prefix).
func (k Kind) Valid() bool { return slices.Contains(kinds, k) }

// LatestKey is the key of the newest result of the kind:
// latest/<kind>.json.
func (k Kind) LatestKey() string { return LatestPrefix + string(k) + ".json" }

// HistoryKey is the key of the result collected at t:
// history/<kind>/<RFC3339, UTC>.json.
func (k Kind) HistoryKey(t time.Time) string {
	return HistoryPrefix + string(k) + "/" + t.UTC().Format(time.RFC3339) + ".json"
}

var shardPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)

// ValidShard reports whether s can be a shard name (a node group, a
// namespace): lower case, digits, dots and dashes, at most 64 characters, so a
// shard never builds a key that leaves the kind's prefix.
func ValidShard(s string) bool { return s != indexShard && shardPattern.MatchString(s) }

// ShardLatestKey is the key of one shard of a kind that grows with the
// cluster: latest/<kind>/<shard>.json (the index is
// latest/<kind>/index.json, IndexKey). The shard must be ValidShard.
func (k Kind) ShardLatestKey(shard string) string {
	return LatestPrefix + string(k) + "/" + shard + ".json"
}

// ShardHistoryKey is the history key of a shard collected at t:
// history/<kind>/<shard>/<RFC3339, UTC>.json.
func (k Kind) ShardHistoryKey(shard string, t time.Time) string {
	return HistoryPrefix + string(k) + "/" + shard + "/" + t.UTC().Format(time.RFC3339) + ".json"
}

// metaCollectedAt is the user metadata of an object that says when it was
// collected (RFC 3339, UTC); a reader falls back to the object's
// last-modified time.
const metaCollectedAt = "collected-at"
