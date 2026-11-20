package cloudstore

import (
	"context"
	"fmt"
	"regexp"
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/results"
)

// resultBucketPattern is what a results bucket is named
// (crossplaneaws.ResultsBucketName): the tenant role may only reach cv-*
// buckets, and a summary naming another is refused.
var resultBucketPattern = regexp.MustCompile(`^cv-[a-z0-9][a-z0-9-]*[a-z0-9]$`)

// SetResultRef writes the summary of one kind of scan result, replacing
// that kind's; see repository.Clusters.
func (r clusters) SetResultRef(ctx context.Context, tenantID, id string, ref model.ResultRef) error {
	if err := validResultRef(ref); err != nil {
		return fmt.Errorf("%w: %w", repository.ErrInvalid, err)
	}
	next := resultRefToAPI(ref)
	return translate(retry(func() error {
		obj, err := r.find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		before := obj.Status.DeepCopy()
		obj.Status.Results = withResultRef(obj.Status.Results, next)
		dropListsInTheBucket(&obj.Status)
		if equality.Semantic.DeepEqual(before, &obj.Status) {
			return nil
		}
		return r.c.Status().Update(ctx, obj)
	}))
}

func validResultRef(ref model.ResultRef) error {
	kind := results.Kind(ref.Kind)
	switch {
	case !kind.Valid():
		return fmt.Errorf("result kind %q is not one of %v", ref.Kind, results.Kinds())
	case !resultBucketPattern.MatchString(ref.Bucket) || len(ref.Bucket) > 63:
		return fmt.Errorf("result bucket %q is not a results bucket (cv-...)", ref.Bucket)
	case kind.Sharded() && ref.Key != kind.IndexKey():
		return fmt.Errorf("result key %q is not %q", ref.Key, kind.IndexKey())
	case !kind.Sharded() && ref.Key != kind.LatestKey():
		return fmt.Errorf("result key %q is not %q", ref.Key, kind.LatestKey())
	}
	return nil
}

// withResultRef is refs with next in place of the one of its kind, in
// the order of the kinds.
func withResultRef(refs []cloudv1alpha1.ClusterResultRef, next cloudv1alpha1.ClusterResultRef) []cloudv1alpha1.ClusterResultRef {
	out := make([]cloudv1alpha1.ClusterResultRef, 0, len(refs)+1)
	for _, ref := range refs {
		if ref.Kind != next.Kind {
			out = append(out, ref)
		}
	}
	out = append(out, next)
	order := results.Kinds()
	slices.SortFunc(out, func(a, b cloudv1alpha1.ClusterResultRef) int {
		return slices.Index(order, results.Kind(a.Kind)) - slices.Index(order, results.Kind(b.Kind))
	})
	return out
}

// dropListsInTheBucket removes from the status what a summary now holds in
// the bucket: the findings, and the policy snapshot once violations and
// events both are there.
func dropListsInTheBucket(st *cloudv1alpha1.ClusterStatus) {
	has := map[string]bool{}
	for _, ref := range st.Results {
		has[ref.Kind] = true
	}
	if has[cloudv1alpha1.ResultKindDeprecations] && st.Deprecations != nil {
		st.Deprecations.Findings = nil
	}
	if has[cloudv1alpha1.ResultKindViolations] && has[cloudv1alpha1.ResultKindViolationEvents] {
		st.PolicySnapshot = ""
	}
}

func resultRefToAPI(ref model.ResultRef) cloudv1alpha1.ClusterResultRef {
	out := cloudv1alpha1.ClusterResultRef{Kind: ref.Kind, Bucket: ref.Bucket, Key: ref.Key, ETag: ref.ETag, Digest: ref.Digest,
		Total: ref.Total, Removed: ref.Removed, Incomplete: ref.Incomplete, Truncated: ref.Truncated}
	if !ref.CollectedAt.IsZero() {
		out.CollectedAt = &metav1.Time{Time: ref.CollectedAt.UTC()}
	}
	if !ref.AttemptedAt.IsZero() {
		out.AttemptedAt = &metav1.Time{Time: ref.AttemptedAt.UTC()}
	}
	return out
}

// resultRefsFromAPI are the summaries of a status as rows.
func resultRefsFromAPI(refs []cloudv1alpha1.ClusterResultRef) []model.ResultRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]model.ResultRef, 0, len(refs))
	for _, ref := range refs {
		m := model.ResultRef{Kind: ref.Kind, Bucket: ref.Bucket, Key: ref.Key, ETag: ref.ETag, Digest: ref.Digest,
			Total: ref.Total, Removed: ref.Removed, Incomplete: ref.Incomplete, Truncated: ref.Truncated}
		if ref.CollectedAt != nil {
			m.CollectedAt = ref.CollectedAt.UTC()
		}
		if ref.AttemptedAt != nil {
			m.AttemptedAt = ref.AttemptedAt.UTC()
		}
		out = append(out, m)
	}
	return out
}
