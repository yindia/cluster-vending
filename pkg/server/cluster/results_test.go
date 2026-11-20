package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/results"
)

// fakeBucket is S3 for one bucket: a map of objects. It only reads and
// writes (what the server and the writer under test do).
type fakeBucket struct {
	results.S3API
	mu      sync.Mutex
	objects map[string][]byte
	etags   map[string]string
	n       int
	gets    []string
	err     error
}

func (f *fakeBucket) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := make([]byte, 0, 1024)
	buf := make([]byte, 4096)
	for {
		n, err := in.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	key := *in.Bucket + "/" + *in.Key
	f.n++
	f.objects[key], f.etags[key] = body, fmt.Sprintf(`"etag-%d"`, f.n)
	return &s3.PutObjectOutput{ETag: ptr.To(f.etags[key])}, nil
}

func (f *fakeBucket) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := *in.Bucket + "/" + *in.Key
	f.gets = append(f.gets, key)
	if f.err != nil {
		return nil, f.err
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body)), ETag: ptr.To(f.etags[key]), LastModified: ptr.To(time.Now())}, nil
}

// resultsWorld is a world whose service reads results through a fake S3.
type resultsWorld struct {
	world
	svc    *ClusterService
	s3     *fakeBucket
	bucket string
	// factoryCalls is what the store asked the S3 client factory for.
	asked []string
}

func newResultsWorld(t *testing.T) *resultsWorld {
	t.Helper()
	w := newWorld(t, interceptor.Funcs{}, nil)
	rw := &resultsWorld{world: w, s3: &fakeBucket{objects: map[string][]byte{}, etags: map[string]string{}}}
	rw.bucket = crossplaneaws.ResultsBucketName("prod-1a2b3c4d", "444455556666", "us-east-1", w.row.ID)
	store := NewS3Results(func(account *model.CloudAccount, region string) (results.S3API, error) {
		rw.asked = append(rw.asked, account.AccountID+"/"+region)
		return rw.s3, nil
	}, results.NewCache(0))
	rw.svc = NewClusterService(w.svc.deps, WithResults(store))
	return rw
}

// put writes a result as the controller's scanner does and records its
// summary on the cluster.
func (w *resultsWorld) put(t *testing.T, kind results.Kind, msg proto.Message, mutate ...func(*model.ResultRef)) string {
	t.Helper()
	_, etag, err := results.NewWriter(w.s3).Put(t.Context(), w.bucket, kind, msg, time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	ref := model.ResultRef{Kind: string(kind), Bucket: w.bucket, Key: kind.LatestKey(), ETag: etag,
		CollectedAt: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	for _, m := range mutate {
		m(&ref)
	}
	if err := w.store.Clusters().SetResultRef(t.Context(), worldTenant, w.row.ID, ref); err != nil {
		t.Fatal(err)
	}
	return etag
}

func findings(n, removed int) *cloudv1.ClusterDeprecations {
	d := &cloudv1.ClusterDeprecations{TargetVersion: "1.37", Total: int32(n), Removed: int32(removed),
		ScannedAt: timestamppb.New(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))}
	for i := range n {
		d.Findings = append(d.Findings, &cloudv1.DeprecationFinding{Namespace: "web", Kind: "Ingress",
			Name: fmt.Sprintf("f%03d", i), ApiVersion: "extensions/v1beta1", Removed: i < removed})
	}
	return d
}

func TestDeprecationsAreReadFromTheBucketWhenTheSummaryNamesOne(t *testing.T) {
	w := newResultsWorld(t)
	// 500 findings: more than the Cluster object could ever hold.
	w.put(t, results.Deprecations, findings(500, 120), func(r *model.ResultRef) { r.Total, r.Removed = 500, 120 })

	req := &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, PageSize: 200}
	var all []string
	for pages := 0; pages < 10; pages++ {
		res, err := w.svc.ListClusterDeprecations(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.GetTotal() != 500 || res.GetRemoved() != 120 || res.GetTargetVersion() != "1.37" || res.GetError() != "" {
			t.Fatalf("response header = %v", res)
		}
		for _, f := range res.GetFindings() {
			all = append(all, f.GetName())
		}
		if res.GetNextPageToken() == "" {
			break
		}
		req.PageToken = res.GetNextPageToken()
	}
	if len(all) != 500 || all[0] != "f000" || all[499] != "f499" {
		t.Errorf("read %d findings, first %v", len(all), all[:1])
	}
	// Through the tenant role of the cluster's account, in its region.
	if len(w.asked) == 0 || w.asked[0] != "444455556666/us-east-1" {
		t.Errorf("the S3 client was made for %v", w.asked)
	}
	// Only removed.
	removed, err := w.svc.ListClusterDeprecations(t.Context(), &cloudv1.ListClusterDeprecationsRequest{
		TenantId: worldTenant, ClusterId: w.row.ID, OnlyRemoved: true, PageSize: 200})
	if err != nil || len(removed.GetFindings()) != 120 || removed.GetNextPageToken() != "" {
		t.Errorf("only removed: %d findings, next %q, %v", len(removed.GetFindings()), removed.GetNextPageToken(), err)
	}
}

// A token belongs to the object it was made from: another scan's object has
// another ETag, and the list starts over.
func TestDeprecationPageTokenIsBoundToTheObject(t *testing.T) {
	w := newResultsWorld(t)
	w.put(t, results.Deprecations, findings(10, 2))
	req := &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, PageSize: 4}
	first, err := w.svc.ListClusterDeprecations(t.Context(), req)
	if err != nil || first.GetNextPageToken() == "" {
		t.Fatalf("first page = %v, %v", first, err)
	}
	if len(first.GetNextPageToken()) > 8 || strings.Trim(first.GetNextPageToken(), "0123456789") != "" {
		t.Errorf("token %q does not fit the request's page_token (at most 8 digits)", first.GetNextPageToken())
	}
	req.PageToken = first.GetNextPageToken()
	if second, err := w.svc.ListClusterDeprecations(t.Context(), req); err != nil || second.GetFindings()[0].GetName() != "f004" {
		t.Fatalf("second page = %v, %v", second, err)
	}

	w.put(t, results.Deprecations, findings(10, 2)) // a newer scan: same size, another object
	_, err = w.svc.ListClusterDeprecations(t.Context(), req)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a token of the previous object: %v, want InvalidArgument", err)
	}
	for _, bad := range []string{"abc", "99999999"} {
		req.PageToken = bad
		if _, err := w.svc.ListClusterDeprecations(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("token %q: %v, want InvalidArgument", bad, err)
		}
	}
}

// Whatever goes wrong with the bucket is a coverage notice in the answer,
// never a failed call.
func TestDeprecationsWhenTheBucketCannotBeRead(t *testing.T) {
	for name, tt := range map[string]struct {
		err    error
		notice string
	}{
		"denied":  {&smithy.GenericAPIError{Code: "AccessDenied"}, "access to the results bucket was denied"},
		"gone":    {&smithy.GenericAPIError{Code: "NoSuchKey"}, "none have been stored"},
		"timeout": {context.DeadlineExceeded, "timed out"},
		"other":   {errors.New("connection reset"), "could not be read"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newResultsWorld(t)
			w.put(t, results.Deprecations, findings(3, 1), func(r *model.ResultRef) { r.Total, r.Removed = 3, 1 })
			w.s3.err = tt.err
			res, err := w.svc.ListClusterDeprecations(t.Context(), &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
			if err != nil {
				t.Fatalf("the call failed: %v", err)
			}
			if !strings.HasPrefix(res.GetError(), "results unavailable: ") {
				t.Errorf("error = %q, want a coverage notice", res.GetError())
			}
			// The summary is still there: counts and time.
			if res.GetTotal() != 3 || res.GetRemoved() != 1 {
				t.Errorf("counts = %d/%d, want the summary's", res.GetTotal(), res.GetRemoved())
			}
		})
	}
}

// Until a scanner writes to the bucket, nothing changes: the bucket is not
// touched and the Cluster's data answers.
func TestWithoutASummaryTheClusterDataAnswers(t *testing.T) {
	w := newResultsWorld(t)
	scanned(t, w.world, 1, 2)
	res, err := w.svc.ListClusterDeprecations(t.Context(), &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	if err != nil || len(res.GetFindings()) != 3 || res.GetError() != "" {
		t.Fatalf("res = %v, %v", res, err)
	}
	if len(w.s3.gets) != 0 || len(w.asked) != 0 {
		t.Errorf("the bucket was read without a summary: %v %v", w.s3.gets, w.asked)
	}
}

func TestASummaryWithoutAStoreSaysSo(t *testing.T) {
	w := newResultsWorld(t)
	w.put(t, results.Deprecations, findings(3, 1))
	noStore := NewClusterService(w.world.svc.deps) // a server with no AWS access
	res, err := noStore.ListClusterDeprecations(t.Context(), &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	if err != nil || !strings.HasPrefix(res.GetError(), "results unavailable: ") {
		t.Errorf("res = %v, %v; want a notice", res, err)
	}
}

// A summary can only send the server to the cluster's own bucket.
func TestASummaryOfAnotherBucketIsNotRead(t *testing.T) {
	w := newResultsWorld(t)
	other := crossplaneaws.ResultsBucketName("prod-1a2b3c4d", "999988887777", "us-east-1", w.row.ID)
	w.bucket = other // a forged bucket, in this tenant's account name space
	w.put(t, results.Deprecations, findings(3, 1))
	res, err := w.svc.ListClusterDeprecations(t.Context(), &cloudv1.ListClusterDeprecationsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	if err != nil || !strings.HasPrefix(res.GetError(), "results unavailable: ") || len(res.GetFindings()) != 0 {
		t.Errorf("res = %v, %v; want a notice and nothing from that bucket", res, err)
	}
	if len(w.s3.gets) != 0 {
		t.Errorf("the other bucket was read: %v", w.s3.gets)
	}
}

func TestAnotherTenantsClusterIsNotFound(t *testing.T) {
	w := newResultsWorld(t)
	w.put(t, results.Deprecations, findings(3, 1))
	_, err := w.svc.ListClusterDeprecations(t.Context(), &cloudv1.ListClusterDeprecationsRequest{TenantId: "other", ClusterId: w.row.ID})
	if connect.CodeOf(err) != connect.CodeNotFound || len(w.s3.gets) != 0 {
		t.Errorf("err = %v, reads %v", err, w.s3.gets)
	}
}

func snapshotOf(violations, events int) *cloudv1.ClusterPolicySnapshot {
	p := &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamppb.New(time.Now())}}
	for i := range violations {
		p.Violations = append(p.Violations, &cloudv1.ClusterViolation{Id: fmt.Sprintf("v%03d", i), Engine: []string{"kyverno", "gatekeeper"}[i%2],
			Resource: &cloudv1.PolicyObjectReference{Namespace: []string{"apps", "other"}[i%2]}})
	}
	for i := range events {
		p.Events = append(p.Events, &cloudv1.ClusterViolationEvent{Id: fmt.Sprintf("e%03d", i), Engine: "kyverno",
			Resource: &cloudv1.PolicyObjectReference{Namespace: "apps"}, LastObservedAt: timestamppb.New(time.Now())})
	}
	return p
}

func TestViolationsAndEventsAreReadFromTheBucket(t *testing.T) {
	w := newResultsWorld(t)
	w.put(t, results.Violations, snapshotOf(300, 0))
	w.put(t, results.ViolationEvents, snapshotOf(0, 7))

	req := &cloudv1.ListClusterViolationsRequest{TenantId: worldTenant, ClusterId: w.row.ID, PageSize: 200, Engine: "kyverno"}
	first, err := w.svc.ListClusterViolations(t.Context(), req)
	if err != nil || len(first.Violations) != 150 || first.NextPageToken != "" || first.Summary.Incomplete {
		t.Fatalf("violations = %d, next %q, %v (%v)", len(first.GetViolations()), first.GetNextPageToken(), err, first.GetSummary())
	}
	req.Engine, req.PageSize = "", 100
	page, err := w.svc.ListClusterViolations(t.Context(), req)
	if err != nil || len(page.Violations) != 100 || page.NextPageToken == "" {
		t.Fatalf("page = %d, %q, %v", len(page.GetViolations()), page.GetNextPageToken(), err)
	}
	events, err := w.svc.ListClusterViolationEvents(t.Context(), &cloudv1.ListClusterViolationEventsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	if err != nil || len(events.Events) != 7 {
		t.Fatalf("events = %v, %v", events, err)
	}

	// A newer object: the token of the old one is refused.
	w.put(t, results.Violations, snapshotOf(300, 0))
	req.PageToken = page.NextPageToken
	if _, err := w.svc.ListClusterViolations(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a token of the previous object: %v", err)
	}
}

func TestViolationsWhenTheBucketCannotBeRead(t *testing.T) {
	w := newResultsWorld(t)
	w.put(t, results.Violations, snapshotOf(3, 0))
	w.s3.err = &smithy.GenericAPIError{Code: "AccessDenied"}
	res, err := w.svc.ListClusterViolations(t.Context(), &cloudv1.ListClusterViolationsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	if err != nil {
		t.Fatalf("the call failed: %v", err)
	}
	if !res.Summary.Incomplete || !containsNotice(res.Summary.Notices, "results unavailable: access to the results bucket was denied") {
		t.Errorf("summary = %v, want an incomplete summary with the notice", res.Summary)
	}
	events, err := w.svc.ListClusterViolationEvents(t.Context(), &cloudv1.ListClusterViolationEventsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	if err != nil || !events.Summary.Incomplete {
		t.Errorf("events = %v, %v", events, err)
	}
}

func containsNotice(notices []string, want string) bool {
	for _, n := range notices {
		if n == want {
			return true
		}
	}
	return false
}

func recommendationSet(n int) *cloudv1.ListClusterRecommendationsResponse {
	out := &cloudv1.ListClusterRecommendationsResponse{Total: int32(n),
		Summary: &cloudv1.ClusterRecommendations{Workloads: int32(n), Containers: int32(n), MetricsAvailable: true,
			SampledAt: timestamppb.New(time.Now())}}
	for i := range n {
		r := &cloudv1.ResourceRecommendation{Namespace: []string{"a", "b"}[i%2], WorkloadKind: "Deployment", WorkloadName: fmt.Sprintf("w%03d", i),
			Container: "app", CurrentRequests: &cloudv1.ResourceAmounts{CpuMillis: 100},
			RecommendedRequests: &cloudv1.ResourceAmounts{CpuMillis: 100}}
		if i%4 == 0 {
			r.RecommendedRequests.CpuMillis = 300 // changes
		}
		out.Recommendations = append(out.Recommendations, r)
	}
	return out
}

func TestRecommendationsAreReadFromTheBucket(t *testing.T) {
	w := newResultsWorld(t)
	w.put(t, results.Recommendations, recommendationSet(40))
	list := func(req *cloudv1.ListClusterRecommendationsRequest) *cloudv1.ListClusterRecommendationsResponse {
		t.Helper()
		req.TenantId, req.ClusterId = worldTenant, w.row.ID
		res, err := w.svc.ListClusterRecommendations(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := list(&cloudv1.ListClusterRecommendationsRequest{PageSize: 15}); len(res.Recommendations) != 15 || res.Total != 40 ||
		res.NextPageToken == "" || !res.Summary.MetricsAvailable {
		t.Errorf("first page = %d, total %d, next %q, summary %v", len(res.Recommendations), res.Total, res.NextPageToken, res.Summary)
	}
	if res := list(&cloudv1.ListClusterRecommendationsRequest{Namespace: "a"}); res.Total != 20 || len(res.Recommendations) != 20 {
		t.Errorf("namespace a: %d of %d", len(res.Recommendations), res.Total)
	}
	if res := list(&cloudv1.ListClusterRecommendationsRequest{OnlyChanges: true}); res.Total != 10 || len(res.Recommendations) != 10 {
		t.Errorf("only changes: %d of %d", len(res.Recommendations), res.Total)
	}

	first := list(&cloudv1.ListClusterRecommendationsRequest{PageSize: 15})
	w.put(t, results.Recommendations, recommendationSet(40))
	_, err := w.svc.ListClusterRecommendations(t.Context(), &cloudv1.ListClusterRecommendationsRequest{
		TenantId: worldTenant, ClusterId: w.row.ID, PageSize: 15, PageToken: first.NextPageToken})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a token of the previous object: %v", err)
	}
}

func TestRecommendationsWhenTheBucketCannotBeRead(t *testing.T) {
	w := newResultsWorld(t)
	w.put(t, results.Recommendations, recommendationSet(3))
	w.s3.err = context.DeadlineExceeded
	res, err := w.svc.ListClusterRecommendations(t.Context(), &cloudv1.ListClusterRecommendationsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	if err != nil || !strings.Contains(res.GetSummary().GetMessage(), "results unavailable: reading them timed out") {
		t.Errorf("res = %v, %v; want the notice in the summary's message", res, err)
	}
}
