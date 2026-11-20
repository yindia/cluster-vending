package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/results"
)

// Where scan results are read from (docs/architecture.md#results-in-s3).
//
// A cluster whose Cluster object has a summary of a kind (model.ResultRef)
// has the full result of that kind in its results bucket: the server reads
// it from there, with the tenant role of the cluster's account, bounded and
// cached (pkg/results). A kind without a summary is read from the Cluster
// object, as before a scanner wrote to the bucket. When the bucket cannot be
// read, the call still answers: with what the Cluster has (the counts of the
// summary) and a coverage notice, never an error.

// ResultStore reads the newest result of a kind from a cluster's results
// bucket, through the tenant role of the cluster's account.
type ResultStore interface {
	Latest(ctx context.Context, account *model.CloudAccount, region, bucket string, kind results.Kind, into proto.Message) (etag string, collected time.Time, err error)
	// Index reads the index of a sharded kind, and LatestShard one of its
	// shards: the API reads the index, then only the shard a page needs.
	Index(ctx context.Context, account *model.CloudAccount, region, bucket string, kind results.Kind) (idx *cloudv1.ResultIndex, etag string, collected time.Time, err error)
	LatestShard(ctx context.Context, account *model.CloudAccount, region, bucket string, kind results.Kind, shard string, into proto.Message) (etag string, collected time.Time, err error)
}

// Option configures the ClusterService.
type Option func(*ClusterService)

// WithResults gives the service where it reads scan results from. Without
// it a cluster whose results are in the bucket is answered with a coverage
// notice and what the Cluster object has.
func WithResults(store ResultStore) Option { return func(s *ClusterService) { s.results = store } }

// S3Results is a ResultStore over S3 clients made per account: the reader
// of every call shares one cache of what was read (pkg/results.Cache).
type S3Results struct {
	clientFor func(account *model.CloudAccount, region string) (results.S3API, error)
	cache     *results.Cache
}

// NewS3Results returns a store that reads with the client clientFor makes
// for an account and region, over cache.
func NewS3Results(clientFor func(*model.CloudAccount, string) (results.S3API, error), cache *results.Cache) *S3Results {
	return &S3Results{clientFor: clientFor, cache: cache}
}

// NewAWSResults is the store of the API server: clients that call as the
// tenant role of the account (pkg/cloud/aws Client.S3), its access made
// from what the account's bootstrap stored. A role the account lacks, or one
// of another account, is access denied.
func NewAWSResults(c *awscloud.Client) *S3Results {
	return NewS3Results(func(a *model.CloudAccount, region string) (results.S3API, error) {
		access, err := awscloud.AccessFor(a.AccountID, a.Identity, a.ExternalID)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", results.ErrAccessDenied, err)
		}
		client, err := c.S3(access, region)
		if err != nil {
			return nil, err
		}
		return client, nil
	}, results.NewCache(0))
}

// Latest implements ResultStore.
func (s *S3Results) Latest(ctx context.Context, account *model.CloudAccount, region, bucket string, kind results.Kind, into proto.Message) (string, time.Time, error) {
	client, err := s.clientFor(account, region)
	if err != nil {
		return "", time.Time{}, err
	}
	return results.NewReader(client, results.WithCache(s.cache)).Latest(ctx, bucket, kind, into)
}

// Index implements ResultStore.
func (s *S3Results) Index(ctx context.Context, account *model.CloudAccount, region, bucket string, kind results.Kind) (*cloudv1.ResultIndex, string, time.Time, error) {
	client, err := s.clientFor(account, region)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	return results.NewReader(client, results.WithCache(s.cache)).Index(ctx, bucket, kind)
}

// LatestShard implements ResultStore.
func (s *S3Results) LatestShard(ctx context.Context, account *model.CloudAccount, region, bucket string, kind results.Kind, shard string, into proto.Message) (string, time.Time, error) {
	client, err := s.clientFor(account, region)
	if err != nil {
		return "", time.Time{}, err
	}
	return results.NewReader(client, results.WithCache(s.cache)).LatestShard(ctx, bucket, kind, shard, into)
}

// bucketRead is what reading a kind of a cluster's results came to.
type bucketRead struct {
	// Used: the result was read from the bucket, into the message given;
	// ETag is the object's and Collected its time.
	Used      bool
	ETag      string
	Collected time.Time
	// Notice, when not empty, says why a result that is in the bucket was
	// not read: the coverage notice of the answer.
	Notice string
}

// Notices of what is not the bucket's fault.
const (
	noticeNoStore  = "results unavailable: this API server is not set up to read them"
	noticeNotOurs  = "results unavailable: the stored location is not this cluster's results bucket"
	noticeNoAccess = "results unavailable: the cluster's account could not be read"
)

// readResult reads the cluster's result of kind into into when its summary
// names the bucket's object. Without a summary it reads nothing and says
// nothing: the Cluster object's data answers.
func (s *ClusterService) readResult(ctx context.Context, row *model.Cluster, kind results.Kind, into proto.Message) bucketRead {
	ref := row.ResultRef(string(kind))
	if ref == nil {
		return bucketRead{}
	}
	account, notice := s.resultAccount(ctx, row, ref, kind)
	if notice != "" {
		return bucketRead{Notice: notice}
	}
	etag, collected, err := s.results.Latest(ctx, account, row.Region, ref.Bucket, kind, into)
	if err != nil {
		s.logger().Warn("cluster results: not read", "cluster", row.ID, "kind", kind, "bucket", ref.Bucket, "error", err)
		return bucketRead{Notice: results.Notice(err)}
	}
	return bucketRead{Used: true, ETag: etag, Collected: collected}
}

// resultAccount is the account whose tenant role reads the summary's bucket,
// or the coverage notice that says why it cannot be read: the server has no
// store, the account is unreadable, or the summary names a bucket that is not
// this cluster's own.
func (s *ClusterService) resultAccount(ctx context.Context, row *model.Cluster, ref *model.ResultRef, kind results.Kind) (*model.CloudAccount, string) {
	if s.results == nil {
		return nil, noticeNoStore
	}
	account, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, row.TenantID, row.CloudAccountID)
	if err != nil {
		s.logger().Warn("cluster results: account not read", "cluster", row.ID, "kind", kind, "error", err)
		return nil, noticeNoAccess
	}
	// The summary is written by the controller, but only this cluster's own
	// bucket is ever read with its account's role.
	if !crossplaneaws.ResultsBucketBelongsTo(ref.Bucket, account.AccountID, row.Region, row.ID) {
		s.logger().Warn("cluster results: summary names another bucket", "cluster", row.ID, "kind", kind, "bucket", ref.Bucket)
		return nil, noticeNotOurs
	}
	return account, ""
}

// A page token of a list read from the bucket carries the object it was
// made from: the offset in the high bits, ten bits of the ETag's hash in the
// low ones, written as the decimal number the request's page_token takes
// (digits, at most 8). Another object (another ETag) refuses the token, and
// the list starts over.
const tokenETagBits = 10

func etagBits(etag string) int {
	sum := sha256.Sum256([]byte(etag))
	return int(binary.BigEndian.Uint16(sum[:2])) & (1<<tokenETagBits - 1)
}

func etagToken(etag string, offset int) string {
	return strconv.Itoa(offset<<tokenETagBits | etagBits(etag))
}

// etagOffset is the offset of a token made from the object with etag; 0 for
// no token.
func etagOffset(etag, token string, n int) (int, error) {
	if token == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(token)
	if err != nil || v < 0 || v&(1<<tokenETagBits-1) != etagBits(etag) || v>>tokenETagBits > n {
		return 0, connect.NewError(connect.CodeInvalidArgument, "page_token does not match this result; restart the list")
	}
	return v >> tokenETagBits, nil
}
