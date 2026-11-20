package results

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/protobuf/proto"
)

// Writer writes the results of a scan. Safe for concurrent use.
type Writer struct {
	s3       S3API
	maxBytes int
	timeout  time.Duration
}

// NewWriter returns a Writer that writes with client.
func NewWriter(client S3API, opts ...Option) *Writer {
	o := settle(opts, DefaultWriteTimeout)
	return &Writer{s3: client, maxBytes: o.maxBytes, timeout: o.timeout}
}

// Put stores msg as the result of kind collected at now: first the history
// object history/<kind>/<RFC3339>.json, then latest/<kind>.json over the
// previous one, so a reader of latest never sees a result the history lacks.
// It returns the key and the ETag of latest, which the cluster's summary
// names. A message above the size bound (ErrTooLarge) writes nothing; if the
// history write fails, latest is left as it was.
// WithoutHistory leaves out the history object: only latest is written.
func (w *Writer) Put(ctx context.Context, bucket string, kind Kind, msg proto.Message, now time.Time, opts ...PutOption) (key, etag string, err error) {
	if !kind.Valid() {
		return "", "", errors.New("results: a bucket and a known kind are required")
	}
	historyKey := kind.HistoryKey(now)
	if putOptionsOf(opts).noHistory {
		historyKey = ""
	}
	return w.write(ctx, bucket, historyKey, kind.LatestKey(), msg, now)
}

// PutOption configures Put.
type PutOption func(*putOptions)

type putOptions struct{ noHistory bool }

func putOptionsOf(opts []PutOption) putOptions {
	var o putOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithoutHistory makes Put write latest only, for a result that changes so
// often that a history object per change would be most of the bucket (the
// caller keeps one per hour, say).
func WithoutHistory() PutOption { return func(o *putOptions) { o.noHistory = true } }

// PutShard is Put for one shard of a kind that grows with the cluster (a
// node group's nodes, a namespace's recommendations): the history object
// history/<kind>/<shard>/<RFC3339>.json, then latest/<kind>/<shard>.json. It
// returns the shard's key and ETag, which a later index names.
func (w *Writer) PutShard(ctx context.Context, bucket string, kind Kind, shard string, msg proto.Message, now time.Time) (key, etag string, err error) {
	if !kind.Valid() || !ValidShard(shard) {
		return "", "", errors.New("results: a bucket, a known kind and a valid shard are required")
	}
	return w.write(ctx, bucket, kind.ShardHistoryKey(shard, now), kind.ShardLatestKey(shard), msg, now)
}

func (w *Writer) write(ctx context.Context, bucket, historyKey, latestKey string, msg proto.Message, now time.Time) (key, etag string, err error) {
	if bucket == "" {
		return "", "", errors.New("results: a bucket and a known kind are required")
	}
	body, err := Encode(msg, w.maxBytes)
	if err != nil {
		return "", "", err
	}
	meta := map[string]string{metaCollectedAt: now.UTC().Format(time.RFC3339)}
	if historyKey != "" {
		if _, err := w.put(ctx, bucket, historyKey, body, meta); err != nil {
			return "", "", err
		}
	}
	if etag, err = w.put(ctx, bucket, latestKey, body, meta); err != nil {
		return "", "", err
	}
	return latestKey, etag, nil
}

func (w *Writer) put(ctx context.Context, bucket, key string, body []byte, meta map[string]string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	out, err := w.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: awssdk.String(bucket), Key: awssdk.String(key), Body: bytes.NewReader(body),
		ContentType: awssdk.String("application/json"), ContentEncoding: awssdk.String("gzip"), Metadata: meta,
	})
	if err != nil {
		return "", classify(err, fmt.Sprintf("write s3://%s/%s", bucket, key))
	}
	return awssdk.ToString(out.ETag), nil
}
