package results_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/results"
)

const bucket = "cv-prod-1a2b3c4d-0123456789ab"

var t0 = time.Date(2026, 11, 19, 10, 30, 5, 0, time.UTC)

func snapshot(n int) *cloudv1.ClusterPolicySnapshot {
	p := &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamppb.New(t0)}}
	for i := range n {
		p.Violations = append(p.Violations, &cloudv1.ClusterViolation{Id: fmt.Sprintf("v-%d", i), Engine: "kyverno", Message: "naïve 😀 \"quoted\""})
	}
	return p
}

func TestKeys(t *testing.T) {
	for kind, want := range map[results.Kind][2]string{
		results.Deprecations:    {"latest/deprecations.json", "history/deprecations/2026-11-19T10:30:05Z.json"},
		results.Recommendations: {"latest/recommendations.json", "history/recommendations/2026-11-19T10:30:05Z.json"},
		results.Violations:      {"latest/violations.json", "history/violations/2026-11-19T10:30:05Z.json"},
		results.ViolationEvents: {"latest/violation-events.json", "history/violation-events/2026-11-19T10:30:05Z.json"},
	} {
		if got := kind.LatestKey(); got != want[0] {
			t.Errorf("%s latest = %q, want %q", kind, got, want[0])
		}
		// Any zone: the key is UTC.
		local := t0.In(time.FixedZone("x", 9*3600))
		if got := kind.HistoryKey(local); got != want[1] {
			t.Errorf("%s history = %q, want %q", kind, got, want[1])
		}
		if !kind.Valid() {
			t.Errorf("%s is not valid", kind)
		}
	}
	if results.Kind("../etc").Valid() || results.Kind("").Valid() {
		t.Error("an unknown kind is valid")
	}
	if len(results.Kinds()) != 5 {
		t.Errorf("Kinds() = %v", results.Kinds())
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := snapshot(3)
	raw, err := results.Encode(in, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatal("not gzip")
	}
	out := &cloudv1.ClusterPolicySnapshot{}
	if err := results.Decode(bytes.NewReader(raw), 0, out); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, out) {
		t.Errorf("round trip\n got %v\nwant %v", out, in)
	}
}

func TestEncodeRefusesAboveTheBound(t *testing.T) {
	if _, err := results.Encode(snapshot(200), 1024); !errors.Is(err, results.ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
	if _, err := results.Encode(nil, 0); err == nil {
		t.Error("a nil message was encoded")
	}
}

func gz(t *testing.T, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(body); err != nil || w.Close() != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDecodeBoundsWhatItInflates(t *testing.T) {
	// 4 MiB of spaces compress to a few KiB: a bomb against a 1 KiB bound.
	bomb := gz(t, append([]byte(`{"violations":[]}`), bytes.Repeat([]byte(" "), 4<<20)...))
	if len(bomb) > 64<<10 {
		t.Fatalf("test bomb is %d bytes", len(bomb))
	}
	if err := results.Decode(bytes.NewReader(bomb), 1024, &cloudv1.ClusterPolicySnapshot{}); !errors.Is(err, results.ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestDecodeCorrupt(t *testing.T) {
	for name, raw := range map[string][]byte{
		"not gzip":     []byte(`{"summary":{}}`),
		"empty":        nil,
		"not json":     gz(t, []byte("not json")),
		"wrong schema": gz(t, []byte(`{"violations":"x"}`)),
		"truncated":    gz(t, []byte(`{"summary":{}}`))[:12],
	} {
		if err := results.Decode(bytes.NewReader(raw), 0, &cloudv1.ClusterPolicySnapshot{}); !errors.Is(err, results.ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
}

func TestDecodeIgnoresFieldsOfANewerBuild(t *testing.T) {
	raw := gz(t, []byte(`{"summary":{"incomplete":true},"fieldFromTheFuture":[1,2]}`))
	out := &cloudv1.ClusterPolicySnapshot{}
	if err := results.Decode(bytes.NewReader(raw), 0, out); err != nil || !out.GetSummary().GetIncomplete() {
		t.Errorf("Decode = %v, %v; want the known fields", out, err)
	}
}

func TestPutWritesHistoryThenLatest(t *testing.T) {
	s3 := newFake(bucket)
	w := results.NewWriter(s3)
	key, etag, err := w.Put(context.Background(), bucket, results.Violations, snapshot(2), t0)
	if err != nil {
		t.Fatal(err)
	}
	if key != "latest/violations.json" || etag == "" {
		t.Errorf("Put = %q, %q", key, etag)
	}
	if want := []string{bucket + "/history/violations/2026-11-19T10:30:05Z.json", bucket + "/latest/violations.json"}; strings.Join(s3.puts, ",") != strings.Join(want, ",") {
		t.Errorf("writes %v, want history first, then latest: %v", s3.puts, want)
	}
	latest := s3.objects[bucket][key]
	if latest.etag != etag || latest.contentEncoding != "gzip" || latest.contentType != "application/json" ||
		latest.metadata["collected-at"] != "2026-11-19T10:30:05Z" {
		t.Errorf("latest = %+v, etag %q", latest, etag)
	}
	if !bytes.Equal(latest.body, s3.objects[bucket]["history/violations/2026-11-19T10:30:05Z.json"].body) {
		t.Error("history and latest differ")
	}
}

func TestPutRefusals(t *testing.T) {
	for name, call := range map[string]func(w *results.Writer) error{
		"unknown kind": func(w *results.Writer) error {
			_, _, err := w.Put(context.Background(), bucket, "nope", snapshot(1), t0)
			return err
		},
		"no bucket": func(w *results.Writer) error {
			_, _, err := w.Put(context.Background(), "", results.Violations, snapshot(1), t0)
			return err
		},
		"nil message": func(w *results.Writer) error {
			_, _, err := w.Put(context.Background(), bucket, results.Violations, nil, t0)
			return err
		},
	} {
		s3 := newFake(bucket)
		if err := call(results.NewWriter(s3)); err == nil {
			t.Errorf("%s: no error", name)
		}
		if len(s3.puts) != 0 {
			t.Errorf("%s: wrote %v", name, s3.puts)
		}
	}
}

func TestPutTooLargeWritesNothing(t *testing.T) {
	s3 := newFake(bucket)
	_, _, err := results.NewWriter(s3, results.WithMaxBytes(512)).Put(context.Background(), bucket, results.Violations, snapshot(100), t0)
	if !errors.Is(err, results.ErrTooLarge) || len(s3.puts) != 0 {
		t.Errorf("err = %v, writes %v; want ErrTooLarge and no write", err, s3.puts)
	}
}

func TestPutFailureOfHistoryLeavesLatestAlone(t *testing.T) {
	s3 := newFake(bucket)
	w := results.NewWriter(s3)
	if _, _, err := w.Put(context.Background(), bucket, results.Violations, snapshot(1), t0); err != nil {
		t.Fatal(err)
	}
	before := s3.objects[bucket]["latest/violations.json"].etag
	s3.fail = func(op, _, key string) error {
		if op == "PutObject" && strings.HasPrefix(key, "history/") {
			return &smithy.GenericAPIError{Code: "AccessDenied"}
		}
		return nil
	}
	_, _, err := w.Put(context.Background(), bucket, results.Violations, snapshot(5), t0.Add(time.Hour))
	if !errors.Is(err, results.ErrAccessDenied) {
		t.Errorf("err = %v, want ErrAccessDenied", err)
	}
	if s3.objects[bucket]["latest/violations.json"].etag != before {
		t.Error("latest was overwritten although the history write failed")
	}
}

func TestReadAfterWrite(t *testing.T) {
	s3 := newFake(bucket)
	ctx := context.Background()
	want := snapshot(4)
	_, etag, err := results.NewWriter(s3).Put(ctx, bucket, results.Violations, want, t0)
	if err != nil {
		t.Fatal(err)
	}
	got := &cloudv1.ClusterPolicySnapshot{}
	gotETag, collected, err := results.NewReader(s3).Latest(ctx, bucket, results.Violations, got)
	if err != nil {
		t.Fatal(err)
	}
	if gotETag != etag || !collected.Equal(t0) || !proto.Equal(got, want) {
		t.Errorf("Latest = %q, %v, %v; want %q, %v, %v", gotETag, collected, got, etag, t0, want)
	}
}

func TestLatestCollectedFallsBackToLastModified(t *testing.T) {
	s3 := newFake(bucket)
	raw, _ := results.Encode(snapshot(1), 0)
	mod := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s3.objects[bucket]["latest/violations.json"] = &fakeObject{body: raw, etag: `"e1"`, modified: mod}
	_, collected, err := results.NewReader(s3).Latest(context.Background(), bucket, results.Violations, &cloudv1.ClusterPolicySnapshot{})
	if err != nil || !collected.Equal(mod) {
		t.Errorf("collected = %v, %v; want the last-modified time", collected, err)
	}
}

func TestLatestErrorsAreTyped(t *testing.T) {
	s3 := newFake(bucket)
	r := results.NewReader(s3)
	ctx := context.Background()
	into := &cloudv1.ClusterPolicySnapshot{}

	// No object yet, and no bucket at all.
	if _, _, err := r.Latest(ctx, bucket, results.Violations, into); !errors.Is(err, results.ErrNotFound) {
		t.Errorf("missing object: %v, want ErrNotFound", err)
	}
	if _, _, err := r.Latest(ctx, "cv-other", results.Violations, into); !errors.Is(err, results.ErrNotFound) {
		t.Errorf("missing bucket: %v, want ErrNotFound", err)
	}
	// Denied: the tenant role was removed, or its credentials expired.
	for _, code := range []string{"AccessDenied", "AccessDeniedException", "ExpiredToken", "InvalidAccessKeyId", "Forbidden"} {
		s3.fail = func(string, string, string) error { return &smithy.GenericAPIError{Code: code} }
		if _, _, err := r.Latest(ctx, bucket, results.Violations, into); !errors.Is(err, results.ErrAccessDenied) {
			t.Errorf("%s: %v, want ErrAccessDenied", code, err)
		}
	}
	// Anything else is passed up as it is, not mistaken for a typed case.
	s3.fail = func(string, string, string) error { return errors.New("connection reset") }
	if _, _, err := r.Latest(ctx, bucket, results.Violations, into); err == nil ||
		errors.Is(err, results.ErrNotFound) || errors.Is(err, results.ErrAccessDenied) || errors.Is(err, results.ErrTimeout) {
		t.Errorf("other: %v", err)
	}
	// An object that is not what the writer writes.
	s3.fail = nil
	s3.objects[bucket]["latest/violations.json"] = &fakeObject{body: []byte("garbage"), etag: `"x"`}
	if _, _, err := r.Latest(ctx, bucket, results.Violations, into); !errors.Is(err, results.ErrCorrupt) {
		t.Errorf("garbage: %v, want ErrCorrupt", err)
	}
}

func TestLatestTimesOut(t *testing.T) {
	s3 := newFake(bucket)
	s3.block = true
	r := results.NewReader(s3, results.WithTimeout(20*time.Millisecond))
	start := time.Now()
	_, _, err := r.Latest(context.Background(), bucket, results.Violations, &cloudv1.ClusterPolicySnapshot{})
	if !errors.Is(err, results.ErrTimeout) || time.Since(start) > 2*time.Second {
		t.Errorf("err = %v after %v, want ErrTimeout at the bound", err, time.Since(start))
	}
	// The caller's own cancellation is not a timeout of the store.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.Latest(ctx, bucket, results.Violations, &cloudv1.ClusterPolicySnapshot{}); errors.Is(err, results.ErrTimeout) {
		t.Errorf("cancelled call: %v, must not say timeout", err)
	}
}

func TestLatestRefusesAnOversizeObject(t *testing.T) {
	s3 := newFake(bucket)
	if _, _, err := results.NewWriter(s3).Put(context.Background(), bucket, results.Violations, snapshot(100), t0); err != nil {
		t.Fatal(err)
	}
	_, _, err := results.NewReader(s3, results.WithMaxBytes(256)).Latest(context.Background(), bucket, results.Violations, &cloudv1.ClusterPolicySnapshot{})
	if !errors.Is(err, results.ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestLatestCacheIsKeyedByETag(t *testing.T) {
	s3 := newFake(bucket)
	ctx := context.Background()
	w, r := results.NewWriter(s3), results.NewReader(s3)
	if _, _, err := w.Put(ctx, bucket, results.Violations, snapshot(2), t0); err != nil {
		t.Fatal(err)
	}
	read := func() (*cloudv1.ClusterPolicySnapshot, string) {
		t.Helper()
		out := &cloudv1.ClusterPolicySnapshot{}
		etag, _, err := r.Latest(ctx, bucket, results.Violations, out)
		if err != nil {
			t.Fatal(err)
		}
		return out, etag
	}
	first, e1 := read()
	again, e2 := read() // a conditional GET answered 304: served from the cache
	if e1 != e2 || !proto.Equal(first, again) || len(again.Violations) != 2 {
		t.Fatalf("cached read = %v %q, first %v %q", again, e2, first, e1)
	}
	// The caller may change what it was given without changing the cache.
	again.Violations = nil
	if third, _ := read(); len(third.Violations) != 2 {
		t.Error("the cache was changed through a caller's message")
	}
	// A newer object has another ETag and is read in full.
	if _, _, err := w.Put(ctx, bucket, results.Violations, snapshot(5), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	next, e3 := read()
	if e3 == e1 || len(next.Violations) != 5 {
		t.Errorf("after a new write: %d violations, etag %q (was %q)", len(next.Violations), e3, e1)
	}
}

func TestLatestCacheIsPerBucketAndKind(t *testing.T) {
	a, b := "cv-a-0123456789ab", "cv-b-0123456789ab"
	s3 := newFake(a, b)
	ctx := context.Background()
	w, r := results.NewWriter(s3), results.NewReader(s3)
	for bkt, n := range map[string]int{a: 1, b: 3} {
		if _, _, err := w.Put(ctx, bkt, results.Violations, snapshot(n), t0); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		for bkt, n := range map[string]int{a: 1, b: 3} {
			out := &cloudv1.ClusterPolicySnapshot{}
			if _, _, err := r.Latest(ctx, bkt, results.Violations, out); err != nil || len(out.Violations) != n {
				t.Fatalf("%s: %d violations, %v; want %d", bkt, len(out.Violations), err, n)
			}
		}
	}
	// Another message type under the same bucket and key must not be
	// answered from a snapshot's entry.
	dep := &cloudv1.ClusterDeprecations{}
	raw, _ := results.Encode(&cloudv1.ClusterDeprecations{Total: 7}, 0)
	s3.objects[a]["latest/violations.json"] = &fakeObject{body: raw, etag: s3.objects[a]["latest/violations.json"].etag}
	if _, _, err := r.Latest(ctx, a, results.Violations, dep); err != nil || dep.Total != 7 {
		t.Errorf("deprecations read = %v, %v", dep, err)
	}
}

func TestCacheStaysBounded(t *testing.T) {
	buckets := make([]string, 10)
	for i := range buckets {
		buckets[i] = fmt.Sprintf("cv-c%d-0123456789ab", i)
	}
	s3 := newFake(buckets...)
	ctx := context.Background()
	w, r := results.NewWriter(s3), results.NewReader(s3, results.WithCacheEntries(3))
	for _, b := range buckets {
		if _, _, err := w.Put(ctx, b, results.Violations, snapshot(1), t0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.Latest(ctx, b, results.Violations, &cloudv1.ClusterPolicySnapshot{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.CachedEntries(); n != 3 {
		t.Errorf("cache holds %d entries, want 3", n)
	}
}

func TestEmptyBucketDeletesEverythingUnderTheResultPrefixes(t *testing.T) {
	s3 := newFake(bucket)
	s3.pageSize = 7
	for i := range 2500 {
		s3.objects[bucket][fmt.Sprintf("history/violations/%05d.json", i)] = &fakeObject{}
	}
	s3.objects[bucket]["latest/violations.json"] = &fakeObject{}
	s3.objects[bucket]["latest/deprecations.json"] = &fakeObject{}
	s3.objects[bucket]["history/deprecations/a.json"] = &fakeObject{}
	n, err := results.EmptyBucket(context.Background(), s3, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2503 || len(s3.keys(bucket)) != 0 {
		t.Errorf("deleted %d, %d objects left", n, len(s3.keys(bucket)))
	}
}

func TestEmptyBucketLeavesForeignObjectsAndSaysSo(t *testing.T) {
	s3 := newFake(bucket)
	s3.objects[bucket]["latest/violations.json"] = &fakeObject{}
	s3.objects[bucket]["somebody-elses.txt"] = &fakeObject{}
	n, err := results.EmptyBucket(context.Background(), s3, bucket)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if got := s3.keys(bucket); len(got) != 1 || got[0] != "somebody-elses.txt" {
		t.Errorf("left %v, want only the foreign object", got)
	}
}

func TestEmptyBucketOfABucketThatIsGoneIsDone(t *testing.T) {
	n, err := results.EmptyBucket(context.Background(), newFake(), bucket)
	if err != nil || n != 0 {
		t.Errorf("EmptyBucket = %d, %v; want nothing to do", n, err)
	}
}

func TestEmptyBucketReportsWhatItCouldNotDelete(t *testing.T) {
	s3 := newFake(bucket)
	s3.objects[bucket]["latest/violations.json"] = &fakeObject{}
	s3.objects[bucket]["latest/deprecations.json"] = &fakeObject{}
	s3.fail = func(op, _, _ string) error {
		if op == "DeleteObject:latest/violations.json" {
			return errors.New("denied")
		}
		return nil
	}
	n, err := results.EmptyBucket(context.Background(), s3, bucket)
	if err == nil || !errors.Is(err, results.ErrAccessDenied) || n != 1 {
		t.Errorf("EmptyBucket = %d, %v; want one deleted and an access denied error", n, err)
	}
	s3.fail = func(op, _, _ string) error {
		if op == "ListObjectsV2" {
			return &smithy.GenericAPIError{Code: "AccessDenied"}
		}
		return nil
	}
	if _, err := results.EmptyBucket(context.Background(), s3, bucket); !errors.Is(err, results.ErrAccessDenied) {
		t.Errorf("list denied: %v", err)
	}
}

func TestNotice(t *testing.T) {
	for err, want := range map[error]string{
		results.ErrNotFound:     "results unavailable: none have been stored for this cluster yet",
		results.ErrAccessDenied: "results unavailable: access to the results bucket was denied",
		results.ErrTimeout:      "results unavailable: reading them timed out",
		results.ErrTooLarge:     "results unavailable: the stored results are larger than the platform reads",
		results.ErrCorrupt:      "results unavailable: the stored results cannot be read",
		errors.New("boom"):      "results unavailable: they could not be read",
	} {
		wrapped := fmt.Errorf("read: %w", err)
		if got := results.Notice(wrapped); got != want {
			t.Errorf("Notice(%v) = %q, want %q", err, got, want)
		}
	}
	if results.Notice(nil) != "" {
		t.Error("Notice(nil) is not empty")
	}
}

// The API server reads results and nothing else: its tenant-role sessions are
// limited to this policy, whatever the role allows.
func TestReadOnlySessionPolicy(t *testing.T) {
	var doc struct {
		Statement []struct {
			Effect   string
			Action   []string
			Resource string
		}
	}
	if err := json.Unmarshal([]byte(results.ReadOnlySessionPolicy), &doc); err != nil {
		t.Fatal(err)
	}
	if len(results.ReadOnlySessionPolicy) > 2048 {
		t.Errorf("the policy is %d characters, STS takes 2048", len(results.ReadOnlySessionPolicy))
	}
	granted := map[string]string{}
	for _, st := range doc.Statement {
		if st.Effect != "Allow" {
			t.Errorf("statement %+v", st)
		}
		for _, a := range st.Action {
			granted[a] = st.Resource
		}
	}
	// ListBucket too: without it S3 answers a missing object with 403, not
	// 404, and a result not written yet would look like a denial.
	want := map[string]string{"s3:GetObject": "arn:aws:s3:::cv-*/*", "s3:ListBucket": "arn:aws:s3:::cv-*"}
	if fmt.Sprint(granted) != fmt.Sprint(want) {
		t.Errorf("granted %v, want exactly %v", granted, want)
	}
}
