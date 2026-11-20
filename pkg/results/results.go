// Package results stores the scan results of a cluster (deprecated APIs,
// resource recommendations, policy violations and their events) in the S3
// bucket the platform renders with the cluster (docs/architecture.md#results-in-s3;
// the bucket is pkg/crossplane/aws results_bucket.go),
// so a large cluster is not limited by the size of a Kubernetes object.
//
// The controller's scanners write and the API server reads, each with an
// S3 client of the cluster's account made through the tenant role
// (pkg/cloud/aws Client.S3, the way reconcile.Deps.AWSAccess reaches the
// account), so this package knows nothing of credentials: it takes an S3API.
//
//	scanner ──Writer.Put──> history/<kind>/<RFC3339>.json, then latest/<kind>.json
//	server  ──Reader.Latest─> latest/<kind>.json (gzip protojson, ETag cache)
//	delete  ──EmptyBucket──> everything under latest/ and history/
//
// Every object is the protojson of the kind's contract message, gzip
// compressed, and bounded (DefaultMaxBytes uncompressed) on both sides.
// Failures are typed (ErrNotFound, ErrAccessDenied, ErrTimeout, ...) so a
// reader turns them into a coverage notice (Notice) instead of failing the
// call it serves.
package results

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3API is the part of the S3 client the package uses; *s3.Client has it
// and tests use a fake.
type S3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

var _ S3API = (*s3.Client)(nil)

// Defaults of the bounds; each is an option.
const (
	// DefaultMaxBytes bounds an object's size uncompressed, written and read
	// (16 MiB).
	DefaultMaxBytes = 16 << 20
	// DefaultReadTimeout bounds one read (the API server's 5s).
	DefaultReadTimeout = 5 * time.Second
	// DefaultWriteTimeout bounds one write (the history object, then latest).
	DefaultWriteTimeout = 30 * time.Second
	// DefaultCacheEntries is how many objects a cache keeps.
	DefaultCacheEntries = 16
)

type options struct {
	maxBytes     int
	timeout      time.Duration
	cacheEntries int
	cache        *Cache
}

// Option configures a Writer or a Reader.
type Option func(*options)

// WithMaxBytes sets the bound of an object's size uncompressed.
func WithMaxBytes(n int) Option { return func(o *options) { o.maxBytes = n } }

// WithTimeout sets the bound of one call (a read; a write's each object).
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// WithCacheEntries sets how many objects the reader's own cache keeps.
func WithCacheEntries(n int) Option { return func(o *options) { o.cacheEntries = n } }

// WithCache makes the reader use cache, shared with other readers: the API
// server builds a reader per account (each has its own S3 client) over one
// cache.
func WithCache(c *Cache) Option { return func(o *options) { o.cache = c } }

func settle(opts []Option, timeout time.Duration) options {
	o := options{maxBytes: DefaultMaxBytes, timeout: timeout, cacheEntries: DefaultCacheEntries}
	for _, opt := range opts {
		opt(&o)
	}
	if o.maxBytes <= 0 {
		o.maxBytes = DefaultMaxBytes
	}
	if o.timeout <= 0 {
		o.timeout = timeout
	}
	if o.cacheEntries <= 0 {
		o.cacheEntries = DefaultCacheEntries
	}
	return o
}

// ReadOnlySessionPolicy is the session policy of the API server's tenant-role
// sessions (pkg/cloud/aws Config.SessionPolicy): it reads objects of the
// results buckets (cv-*) and lists them, nothing else, whatever the tenant
// role allows. ListBucket is there so that S3 answers an object that is not
// written yet with 404 and not 403.
const ReadOnlySessionPolicy = `{"Version":"2012-10-17","Statement":[` +
	`{"Effect":"Allow","Action":["s3:GetObject"],"Resource":"arn:aws:s3:::cv-*/*"},` +
	`{"Effect":"Allow","Action":["s3:ListBucket"],"Resource":"arn:aws:s3:::cv-*"}]}`
