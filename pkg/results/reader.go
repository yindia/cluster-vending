package results

import (
	"context"
	"errors"
	"fmt"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Reader reads the newest result of a kind. It is cheap: the API server
// makes one per request over the S3 client of the cluster's account
// (WithCache shares what was read). Safe for concurrent use.
type Reader struct {
	s3       S3API
	maxBytes int
	timeout  time.Duration
	cache    *Cache
}

// NewReader returns a Reader that reads with client.
func NewReader(client S3API, opts ...Option) *Reader {
	o := settle(opts, DefaultReadTimeout)
	cache := o.cache
	if cache == nil {
		cache = NewCache(o.cacheEntries)
	}
	return &Reader{s3: client, maxBytes: o.maxBytes, timeout: o.timeout, cache: cache}
}

// CachedEntries is how many objects the reader's cache holds.
func (r *Reader) CachedEntries() int { return r.cache.Len() }

// Latest reads latest/<kind>.json of bucket into into and returns the
// object's ETag (the one a summary names) and when it was collected (its
// collected-at metadata, else its last-modified time). It makes one call,
// bounded by the reader's timeout. A conditional GET with the cached ETag
// is answered without the body while the object has not changed.
//
// Errors: ErrNotFound (no bucket or no object yet), ErrAccessDenied,
// ErrTimeout, ErrTooLarge, ErrCorrupt, or the transport's own. The caller
// makes a coverage notice of them (Notice).
func (r *Reader) Latest(ctx context.Context, bucket string, kind Kind, into proto.Message) (etag string, collected time.Time, err error) {
	if bucket == "" || !kind.Valid() || into == nil {
		return "", time.Time{}, errors.New("results: a bucket, a kind and a message are required")
	}
	return r.read(ctx, bucket, kind.LatestKey(), into)
}

// read is Latest for the object at key.
func (r *Reader) read(ctx context.Context, bucket, key string, into proto.Message) (etag string, collected time.Time, err error) {
	what := fmt.Sprintf("read s3://%s/%s", bucket, key)
	cacheKey := cacheKey(bucket, key, into)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	in := &s3.GetObjectInput{Bucket: awssdk.String(bucket), Key: awssdk.String(key)}
	if cached := r.cache.etagOf(cacheKey); cached != "" {
		in.IfNoneMatch = awssdk.String(cached)
	}
	out, err := r.s3.GetObject(ctx, in)
	if err != nil {
		if notModified(err) {
			if etag, collected, ok := r.cache.get(cacheKey, into); ok {
				return etag, collected, nil
			}
		}
		return "", time.Time{}, classify(err, what)
	}
	defer func() { _ = out.Body.Close() }()
	if err := Decode(out.Body, r.maxBytes, into); err != nil {
		if errors.Is(err, ErrTooLarge) || errors.Is(err, ErrCorrupt) {
			return "", time.Time{}, fmt.Errorf("%s: %w", what, err)
		}
		return "", time.Time{}, classify(err, what)
	}
	etag, collected = awssdk.ToString(out.ETag), collectedAt(out)
	if etag != "" {
		r.cache.put(cacheKey, etag, collected, into)
	}
	return etag, collected, nil
}

// Index reads the index of a sharded kind (latest/<kind>/index.json): the
// shards, when they were collected and whether the collection was complete.
// It returns the index object's ETag and its collected time. Errors as
// Latest's.
func (r *Reader) Index(ctx context.Context, bucket string, kind Kind) (idx *cloudv1.ResultIndex, etag string, collected time.Time, err error) {
	if bucket == "" || !kind.Sharded() {
		return nil, "", time.Time{}, errors.New("results: a bucket and a sharded kind are required")
	}
	idx = &cloudv1.ResultIndex{}
	etag, collected, err = r.read(ctx, bucket, kind.IndexKey(), idx)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	return idx, etag, collected, nil
}

// LatestShard reads latest/<kind>/<shard>.json into into (one call, bounded
// and cached by ETag as Latest) and returns the shard object's ETag and
// collected time.
func (r *Reader) LatestShard(ctx context.Context, bucket string, kind Kind, shard string, into proto.Message) (etag string, collected time.Time, err error) {
	if bucket == "" || !kind.Sharded() || !ValidShard(shard) || into == nil {
		return "", time.Time{}, errors.New("results: a bucket, a sharded kind, a valid shard and a message are required")
	}
	return r.read(ctx, bucket, kind.ShardLatestKey(shard), into)
}

func collectedAt(out *s3.GetObjectOutput) time.Time {
	if v, ok := out.Metadata[metaCollectedAt]; ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UTC()
		}
	}
	return awssdk.ToTime(out.LastModified).UTC()
}
