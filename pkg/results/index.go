package results

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ShardedPut is what PutSharded did.
type ShardedPut struct {
	// Key and ETag are the index object's: what the cluster's summary names.
	Key, ETag string
	// Unchanged: no shard changed, none was removed and the completeness is
	// the same, so nothing was written and Key and ETag are the index's as it
	// stands.
	Unchanged bool
	// Index is the index as it stands after the call.
	Index *cloudv1.ResultIndex
	// Written and Removed are the shards written (changed or new) and deleted.
	Written, Removed []string
}

// ShardOption configures PutSharded.
type ShardOption func(*shardOptions)

type shardOptions struct{ incomplete bool }

// Incomplete marks the index: the collection stopped early, so the shards may
// miss items.
func Incomplete() ShardOption { return func(o *shardOptions) { o.incomplete = true } }

// Digest is the hash of the deterministic encoding of msg. A shard whose
// digest is the stored one is not rewritten, so msg must not carry what
// changes on every collection (its times).
func Digest(msg proto.Message) string {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return "" // no digest: always written
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PutSharded stores a sharded kind: the shards that changed (a shard whose
// Digest is the index's is not rewritten), the removal of the shards that are
// gone, then the index at latest/<kind>/index.json, so a reader of the index
// never names a shard that is not written. A call that changes nothing writes
// nothing (ShardedPut.Unchanged). An unreadable index is as no index: every
// shard is written. A failure leaves the previous index in place; shards
// written before it are newer than it names, which a reader sees by their
// ETag. The count of a shard is the length of its message's first repeated
// field.
func (w *Writer) PutSharded(ctx context.Context, bucket string, kind Kind, shards map[string]proto.Message, now time.Time, opts ...ShardOption) (*ShardedPut, error) {
	var o shardOptions
	for _, opt := range opts {
		opt(&o)
	}
	if bucket == "" || !kind.Sharded() {
		return nil, errors.New("results: a bucket and a sharded kind are required")
	}
	names := make([]string, 0, len(shards))
	for name, msg := range shards {
		if !ValidShard(name) || msg == nil || !msg.ProtoReflect().IsValid() {
			return nil, fmt.Errorf("results: shard %q is not valid or has no content", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)

	old, oldETag := w.currentIndex(ctx, bucket, kind)
	have := map[string]*cloudv1.ResultIndexShard{}
	for _, s := range old.GetShards() {
		have[s.GetShard()] = s
	}
	next := &cloudv1.ResultIndex{Complete: !o.incomplete, CollectedAt: timestamppb.New(now.UTC())}
	out := &ShardedPut{Index: next, Key: kind.IndexKey()}
	for _, name := range names {
		msg := shards[name]
		digest := Digest(msg)
		if prev, ok := have[name]; ok && digest != "" && prev.GetDigest() == digest && prev.GetEtag() != "" {
			next.Shards = append(next.Shards, prev)
			continue
		}
		key, etag, err := w.PutShard(ctx, bucket, kind, name, msg, now)
		if err != nil {
			return nil, err
		}
		next.Shards = append(next.Shards, &cloudv1.ResultIndexShard{Shard: name, Key: key, Etag: etag, Count: shardCount(msg), Digest: digest})
		out.Written = append(out.Written, name)
	}
	var gone []string
	for name := range have {
		if _, ok := shards[name]; !ok {
			gone = append(gone, name)
		}
	}
	slices.Sort(gone)
	if len(gone) > 0 {
		if err := w.deleteShards(ctx, bucket, kind, gone); err != nil {
			return nil, err
		}
		out.Removed = gone
	}
	if old != nil && len(out.Written) == 0 && len(out.Removed) == 0 && old.GetComplete() == next.GetComplete() {
		out.Unchanged, out.Index = true, old
		out.ETag = oldETag
		return out, nil
	}
	body, err := Encode(next, w.maxBytes)
	if err != nil {
		return nil, err
	}
	meta := map[string]string{metaCollectedAt: now.UTC().Format(time.RFC3339)}
	if out.ETag, err = w.put(ctx, bucket, kind.IndexKey(), body, meta); err != nil {
		return nil, err
	}
	return out, nil
}

// shardCount is the length of the message's first repeated field.
func shardCount(msg proto.Message) int32 {
	fields := msg.ProtoReflect().Descriptor().Fields()
	for i := range fields.Len() {
		if fd := fields.Get(i); fd.IsList() {
			return int32(msg.ProtoReflect().Get(fd).List().Len()) //nolint:gosec // bounded by the object's size
		}
	}
	return 0
}

// currentIndex is the stored index; nil when there is none or it cannot be
// read (everything is written again).
func (w *Writer) currentIndex(ctx context.Context, bucket string, kind Kind) (*cloudv1.ResultIndex, string) {
	idx := &cloudv1.ResultIndex{}
	etag, _, err := (&Reader{s3: w.s3, maxBytes: w.maxBytes, timeout: w.timeout, cache: NewCache(1)}).read(ctx, bucket, kind.IndexKey(), idx)
	if err != nil {
		return nil, ""
	}
	return idx, etag
}

// deleteShards removes the latest objects of shards (history expires by the
// bucket's lifecycle).
func (w *Writer) deleteShards(ctx context.Context, bucket string, kind Kind, names []string) error {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	ids := make([]types.ObjectIdentifier, 0, len(names))
	for _, name := range names {
		ids = append(ids, types.ObjectIdentifier{Key: awssdk.String(kind.ShardLatestKey(name))})
	}
	out, err := w.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: awssdk.String(bucket), Delete: &types.Delete{Objects: ids, Quiet: awssdk.Bool(true)}})
	if err != nil {
		return classify(err, "delete shards of s3://"+bucket+"/"+string(kind))
	}
	if len(out.Errors) > 0 {
		e := out.Errors[0]
		return classify(fmt.Errorf("%s: %s", awssdk.ToString(e.Code), awssdk.ToString(e.Message)), "delete s3://"+bucket+"/"+awssdk.ToString(e.Key))
	}
	return nil
}
