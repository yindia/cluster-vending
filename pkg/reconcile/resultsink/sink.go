// Package resultsink writes a scan's full result to the cluster's results
// bucket (docs/design/cluster-results-s3.md) and records its summary on the
// Cluster. It is how the controller's scanners (deprecations,
// recommendations, policy) use S3, and it never fails a scan: when the
// bucket is not there, not Ready or cannot be written, the caller keeps
// putting the full data in the Cluster object, with a notice that says why.
package resultsink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	awss3 "github.com/upbound/provider-aws/v2/apis/namespaced/s3/v1beta1"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/results"
)

// CallTimeout bounds each S3 call (and the lookups before it): a scan is
// never held up by S3.
const CallTimeout = 10 * time.Second

// Sink writes results of clusters. The zero value writes nothing.
type Sink struct {
	deps   reconcile.Deps
	logger *slog.Logger
}

// New returns a Sink on the reconcilers' dependencies: their store, their
// reader of the management cluster, the one tenant-role path
// (Deps.AWSAccess) and the S3 clients (Deps.ResultsS3, nil: nothing is
// written).
func New(d reconcile.Deps, logger *slog.Logger) *Sink {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Sink{deps: d, logger: logger}
}

// Staged is a result written to the bucket, whose summary is not recorded
// yet: record it with Commit once the Cluster object holds what it keeps
// of the scan (recording it drops the lists the bucket now holds).
type Staged struct {
	sink      *Sink
	tenantID  string
	clusterID string
	ref       model.ResultRef
	// Unchanged: the content is that of the last write (same Digest), so
	// nothing was written and the summary only moves its times.
	Unchanged bool
}

// Ref is the summary Commit records.
func (s *Staged) Ref() model.ResultRef { return s.ref }

// Commit records the summary on the Cluster (Clusters.SetResultRef).
func (s *Staged) Commit(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if err := s.sink.deps.Store.Clusters().SetResultRef(ctx, s.tenantID, s.clusterID, s.ref); err != nil {
		return fmt.Errorf("record the %s summary: %w", s.ref.Kind, err)
	}
	return nil
}

// Digest is the hash of the content of a result, for ResultRef.Digest: the
// deterministic encoding of msg. Callers pass the message without what
// changes on every scan (its times), so an unchanged cluster has an
// unchanged digest.
func Digest(msg proto.Message) string {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return "" // no digest: always written
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Stage writes msg as the result of kind collected at now into the cluster's
// bucket (unless summary.Digest equals the last write's: then nothing is
// written), and returns it for Commit, with summary (counts, completeness,
// times) as the cluster's summary of the kind: its Bucket, Key and ETag are
// the write's. When it could not write, it returns nil and a notice for the
// caller to put next to the data it keeps in the Cluster object. No notice
// and nil: the cluster has no bucket by design (an imported cluster) or
// the controller has no S3 (a test), and the Cluster object is all there is.
//
// opts are the writer's (results.WithoutHistory).
func (s *Sink) Stage(ctx context.Context, tenantID, clusterID string, kind results.Kind, msg proto.Message, now time.Time, summary model.ResultRef, opts ...results.PutOption) (*Staged, string) {
	return s.run(ctx, tenantID, clusterID, kind, now, summary, noticeUnavailable,
		func(ctx context.Context, w *results.Writer, bucket string) (string, string, error) {
			return w.Put(ctx, bucket, kind, msg, now, opts...)
		})
}

// StageSharded is Stage for a kind that grows with the cluster (results.Nodes):
// one message per shard, written as results.Writer.PutSharded does (the
// shards that changed, then the index). summary.Digest is set here, from the
// shards and the completeness (results.Incomplete), so the same content as
// the last write writes nothing (Staged.Unchanged); the summary names the
// index. Notices are about the kind's list ("the node list is unavailable:
// ...") because the Cluster object never holds one.
func (s *Sink) StageSharded(ctx context.Context, tenantID, clusterID string, kind results.Kind, shards map[string]proto.Message, now time.Time, summary model.ResultRef, opts ...results.ShardOption) (*Staged, string) {
	if !kind.Sharded() {
		return nil, noticeShards("the result is not a sharded kind")
	}
	summary.Digest = shardsDigest(shards, summary.Incomplete)
	return s.run(ctx, tenantID, clusterID, kind, now, summary, noticeShards,
		func(ctx context.Context, w *results.Writer, bucket string) (string, string, error) {
			res, err := w.PutSharded(ctx, bucket, kind, shards, now, opts...)
			if err != nil {
				return "", "", err
			}
			return res.Key, res.ETag, nil
		})
}

// shardsDigest hashes the shards' digests by name, and the completeness.
func shardsDigest(shards map[string]proto.Message, incomplete bool) string {
	names := make([]string, 0, len(shards))
	for name := range shards {
		names = append(names, name)
	}
	slices.Sort(names)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "incomplete=%t\n", incomplete)
	for _, name := range names {
		_, _ = fmt.Fprintf(h, "%s=%s\n", name, results.Digest(shards[name]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type writeFunc func(ctx context.Context, w *results.Writer, bucket string) (key, etag string, err error)

func (s *Sink) run(ctx context.Context, tenantID, clusterID string, kind results.Kind, now time.Time, summary model.ResultRef, notice func(string) string, write writeFunc) (*Staged, string) {
	if s == nil || s.deps.ResultsS3 == nil || s.deps.Store == nil {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*CallTimeout) // the lookups, a history write and the latest write
	defer cancel()
	staged, why, err := s.stage(ctx, tenantID, clusterID, kind, now, summary, write)
	if err != nil {
		s.logger.WarnContext(ctx, "scan result not written to the results bucket",
			"tenant_id", tenantID, "cluster_id", clusterID, "kind", kind, "error", err)
	}
	if why != "" {
		return staged, notice(why)
	}
	return staged, ""
}

func (s *Sink) stage(ctx context.Context, tenantID, clusterID string, kind results.Kind, now time.Time, summary model.ResultRef, write writeFunc) (*Staged, string, error) {
	row, err := s.deps.Store.Clusters().Get(ctx, tenantID, clusterID)
	if err != nil {
		return nil, "the cluster could not be read", err
	}
	// The same content as the last write: nothing is rewritten; the summary
	// only moves its times.
	if cur := row.ResultRef(string(kind)); cur != nil && summary.Digest != "" && cur.Digest == summary.Digest && cur.Bucket != "" {
		summary.Kind, summary.Bucket, summary.Key, summary.ETag = cur.Kind, cur.Bucket, cur.Key, cur.ETag
		if summary.CollectedAt.IsZero() {
			summary.CollectedAt = now
		}
		return &Staged{sink: s, tenantID: tenantID, clusterID: clusterID, ref: summary, Unchanged: true}, "", nil
	}
	account, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, tenantID, row.CloudAccountID)
	if err != nil {
		return nil, "the cluster's account could not be read", err
	}
	prefix := crossplaneaws.ClusterPrefix(tenantID, clusterID, row.Name)
	namespace := account.Namespace
	if namespace == "" {
		namespace = kube.TenantNamespace(tenantID)
	}
	ready, err := s.bucketReady(ctx, namespace, prefix)
	switch {
	case errors.Is(err, errNoBucket) && row.Imported:
		return nil, "", nil // an imported cluster has no bucket unless it was created by the platform
	case errors.Is(err, errNoBucket):
		return nil, "the results bucket is not created yet", err
	case err != nil:
		return nil, "the results bucket could not be read", err
	case !ready:
		return nil, "the results bucket is not ready yet", errNotReady
	}
	access, err := s.deps.AWSAccess(ctx, account)
	if err != nil {
		return nil, "the cluster's account could not be reached", err
	}
	client, err := s.deps.ResultsS3(access, row.Region)
	if err != nil {
		return nil, "the cluster's account could not be reached", err
	}
	bucket := crossplaneaws.ResultsBucketName(prefix, account.AccountID, row.Region, clusterID)
	key, etag, err := write(ctx, results.NewWriter(client, results.WithTimeout(CallTimeout)), bucket)
	if err != nil {
		return nil, noticeOf(err), err
	}
	summary.Kind, summary.Bucket, summary.Key, summary.ETag = string(kind), bucket, key, etag
	if summary.CollectedAt.IsZero() {
		summary.CollectedAt = now
	}
	return &Staged{sink: s, tenantID: tenantID, clusterID: clusterID, ref: summary}, "", nil
}

var (
	errNoBucket = errors.New("the cluster has no results bucket")
	errNotReady = errors.New("the results bucket is not ready")
)

// bucketReady reads the cluster's Bucket managed resource: errNoBucket when
// there is none, else whether it is Ready (the provider created it).
func (s *Sink) bucketReady(ctx context.Context, namespace, prefix string) (bool, error) {
	b := &awss3.Bucket{}
	err := s.deps.Reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: crossplaneaws.ResultsBucketObjectName(prefix)}, b)
	switch {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return false, errNoBucket
	case err != nil:
		return false, err
	}
	return b.DeletionTimestamp.IsZero() && b.GetCondition("Ready").Status == corev1.ConditionTrue, nil
}

func noticeUnavailable(why string) string {
	return "scan results are kept in the cluster object: " + why
}

func noticeShards(why string) string { return "the node list is unavailable: " + why }

func noticeOf(err error) string {
	switch {
	case errors.Is(err, results.ErrTooLarge):
		return "the result is larger than the platform stores"
	case errors.Is(err, results.ErrAccessDenied):
		return "access to the results bucket was denied"
	case errors.Is(err, results.ErrTimeout):
		return "writing to the results bucket timed out"
	}
	return "writing to the results bucket failed"
}
