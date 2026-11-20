package deprecations_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/deprecation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile/deprecations"
)

var clock = time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC)

// recorder keeps what a scan recorded.
type recorder struct {
	got  *cloudv1.ClusterDeprecations
	err  error
	rows []string
}

func (r *recorder) SetDeprecations(_ context.Context, tenantID, id string, raw model.JSON) error {
	if r.err != nil {
		return r.err
	}
	r.got = &cloudv1.ClusterDeprecations{}
	r.rows = append(r.rows, tenantID+"/"+id)
	return protojson.Unmarshal(raw, r.got)
}

// workload is a cluster with one Ingress applied with a removed API
// version.
type workload struct{ err error }

func (w workload) Resources(context.Context) ([]deprecation.Resource, error) {
	return []deprecation.Resource{{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses", Kind: "Ingress"}}, w.err
}

func (workload) ListMeta(context.Context, deprecation.Resource, int64, string) ([]deprecation.Meta, string, error) {
	return []deprecation.Meta{{Namespace: "web", Name: "site", Annotations: map[string]string{
		deprecation.LastAppliedAnnotation: `{"apiVersion":"extensions/v1beta1","kind":"Ingress"}`}}}, "", nil
}

func (workload) ListHelmSecrets(context.Context, int64, string) ([]deprecation.HelmSecret, string, error) {
	return nil, "", nil
}

func scanner(t *testing.T, rec *recorder, w workload) deprecations.Scanner {
	t.Helper()
	rules, err := deprecation.Default()
	if err != nil {
		t.Fatal(err)
	}
	return deprecations.Scanner{Rules: rules, Recorder: rec, Now: func() time.Time { return clock },
		Connect: func(kubeconfig []byte) (deprecation.Reader, error) {
			if string(kubeconfig) != "kubeconfig" {
				return nil, errors.New("not the cluster's kubeconfig")
			}
			return w, nil
		}}
}

func cluster(last *cloudv1.ClusterDeprecations) deprecations.Cluster {
	return deprecations.Cluster{TenantID: "acme", ID: "c1", Current: "1.36.2", Last: last,
		Kubeconfig: func(context.Context) ([]byte, error) { return []byte("kubeconfig"), nil }}
}

func ago(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(clock.Add(-d)) }

func TestRunScansAndRecords(t *testing.T) {
	rec := &recorder{}

	next, err := scanner(t, rec, workload{}).Run(t.Context(), cluster(nil))

	if err != nil || next != deprecations.Interval {
		t.Fatalf("Run = %v, %v", next, err)
	}
	got := rec.got
	if got.GetTargetVersion() != "1.37" || got.GetTotal() != 1 || got.GetRemoved() != 1 || got.GetError() != "" ||
		!got.GetScannedAt().AsTime().Equal(clock) || !got.GetAttemptedAt().AsTime().Equal(clock) {
		t.Fatalf("recorded %v", got)
	}
	f := got.GetFindings()[0]
	if f.GetNamespace() != "web" || f.GetName() != "site" || f.GetKind() != "Ingress" || !f.GetRemoved() ||
		f.GetSource() != cloudv1.DeprecationSource_DEPRECATION_SOURCE_MANIFEST || f.GetReplacementApi() != "networking.k8s.io/v1" {
		t.Errorf("finding = %v", f)
	}
	if strings.Join(rec.rows, ",") != "acme/c1" {
		t.Errorf("recorded on %v", rec.rows)
	}
}

func TestRunTargetsWhatTheReleaseUpgradesTo(t *testing.T) {
	rec := &recorder{}
	c := cluster(nil)
	c.Current, c.UpgradeTo = "1.35", "1.36"

	if _, err := scanner(t, rec, workload{}).Run(t.Context(), c); err != nil {
		t.Fatal(err)
	}

	if rec.got.GetTargetVersion() != "1.36" {
		t.Fatalf("target = %s", rec.got.GetTargetVersion())
	}
}

func TestRunWaitsUntilAScanIsDue(t *testing.T) {
	tests := []struct {
		name     string
		last     *cloudv1.ClusterDeprecations
		wantScan bool
		wantNext time.Duration
	}{
		{"never scanned", nil, true, deprecations.Interval},
		{"scanned an hour ago", &cloudv1.ClusterDeprecations{TargetVersion: "1.37", AttemptedAt: ago(time.Hour)}, false, 5 * time.Hour},
		{"scanned seven hours ago", &cloudv1.ClusterDeprecations{TargetVersion: "1.37", AttemptedAt: ago(7 * time.Hour)}, true, deprecations.Interval},
		{"the target changed", &cloudv1.ClusterDeprecations{TargetVersion: "1.36", AttemptedAt: ago(time.Minute)}, true, deprecations.Interval},
		{"failed five minutes ago", &cloudv1.ClusterDeprecations{TargetVersion: "1.37", AttemptedAt: ago(5 * time.Minute), Error: "x"}, false, 10 * time.Minute},
		{"failed an hour ago", &cloudv1.ClusterDeprecations{TargetVersion: "1.37", AttemptedAt: ago(time.Hour), Error: "x"}, true, deprecations.Interval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}

			next, err := scanner(t, rec, workload{}).Run(t.Context(), cluster(tt.last))

			if err != nil || next != tt.wantNext || (rec.got != nil) != tt.wantScan {
				t.Fatalf("Run = %v, %v, scanned %v; want %v, scanned %v", next, err, rec.got != nil, tt.wantNext, tt.wantScan)
			}
		})
	}
}

func TestRunRecordsAScanThatFails(t *testing.T) {
	last := &cloudv1.ClusterDeprecations{TargetVersion: "1.37", AttemptedAt: ago(7 * time.Hour), ScannedAt: ago(7 * time.Hour),
		Total: 2, Removed: 1, Findings: []*cloudv1.DeprecationFinding{{Name: "kept"}}}
	noKubeconfig := func(context.Context) ([]byte, error) { return nil, errors.New("Secret prod-kubeconfig not found") }
	tests := []struct {
		name      string
		change    func(*deprecations.Cluster, *workload)
		wantError string
		wantTotal int32
	}{
		{"no kubeconfig", func(c *deprecations.Cluster, _ *workload) { c.Kubeconfig = noKubeconfig },
			"kubeconfig: Secret prod-kubeconfig not found", 2},
		{"the cluster cannot be reached", func(_ *deprecations.Cluster, w *workload) { w.err = errors.New("connection refused") },
			"discover the cluster's resources: connection refused", 2},
		{"not a kubeconfig", func(c *deprecations.Cluster, _ *workload) {
			c.Kubeconfig = func(context.Context) ([]byte, error) { return []byte("expired"), nil }
		}, "not the cluster's kubeconfig", 2},
		{"and the target changed: the old counts are dropped", func(c *deprecations.Cluster, _ *workload) {
			c.Current, c.Kubeconfig = "1.37", noKubeconfig
		}, "kubeconfig: Secret prod-kubeconfig not found", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, c, w := &recorder{}, cluster(last), workload{}
			tt.change(&c, &w)

			next, err := scanner(t, rec, w).Run(t.Context(), c)

			if err != nil || next != deprecations.RetryInterval {
				t.Fatalf("Run = %v, %v: a failed scan is recorded, not returned", next, err)
			}
			got := rec.got
			if got.GetError() != tt.wantError || got.GetTotal() != tt.wantTotal || !got.GetAttemptedAt().AsTime().Equal(clock) {
				t.Fatalf("recorded %v", got)
			}
			if tt.wantTotal > 0 && (len(got.GetFindings()) != 1 || !got.GetScannedAt().AsTime().Equal(last.GetScannedAt().AsTime())) {
				t.Errorf("the last scan that worked was not kept: %v", got)
			}
		})
	}
}

func TestRunWithoutAVersionWaits(t *testing.T) {
	rec := &recorder{}
	c := cluster(nil)
	c.Current = ""

	next, err := scanner(t, rec, workload{}).Run(t.Context(), c)

	if err != nil || next != deprecations.RetryInterval || rec.got != nil {
		t.Fatalf("Run = %v, %v, recorded %v", next, err, rec.got)
	}
}

func TestRunFailsOnlyWhenNothingCanBeRecorded(t *testing.T) {
	rec := &recorder{err: errors.New("conflict")}
	if _, err := scanner(t, rec, workload{}).Run(t.Context(), cluster(nil)); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("Run = %v", err)
	}
	// Shutting down in the middle of a scan says nothing about the cluster.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec = &recorder{}
	if _, err := scanner(t, rec, workload{}).Run(ctx, cluster(nil)); !errors.Is(err, context.Canceled) || rec.got != nil {
		t.Fatalf("Run = %v, recorded %v", err, rec.got)
	}
}

func TestLongErrorsAreCut(t *testing.T) {
	rec, c := &recorder{}, cluster(nil)
	c.Kubeconfig = func(context.Context) ([]byte, error) { return nil, errors.New(strings.Repeat("x", 4000)) }

	if _, err := scanner(t, rec, workload{}).Run(t.Context(), c); err != nil {
		t.Fatal(err)
	}

	if len(rec.got.GetError()) != 512 {
		t.Fatalf("error has %d bytes", len(rec.got.GetError()))
	}
}

// Publish is told of a scan that worked, after it is recorded, with the full
// result (the findings); a scan that failed publishes nothing.
func TestRunPublishesAScanThatWorkedAfterRecordingIt(t *testing.T) {
	rec := &recorder{}
	var published *cloudv1.ClusterDeprecations
	var recordedBefore bool
	s := scanner(t, rec, workload{})
	s.Publish = func(_ context.Context, c deprecations.Cluster, r, full *cloudv1.ClusterDeprecations) {
		published, recordedBefore = r, rec.got != nil
		if len(full.GetFindings()) != len(r.GetFindings()) || full.GetTruncated() {
			t.Errorf("full has %d findings, the recorded %d", len(full.GetFindings()), len(r.GetFindings()))
		}
		if c.ID != "c1" {
			t.Errorf("published for %s", c.ID)
		}
	}

	if _, err := s.Run(t.Context(), cluster(nil)); err != nil {
		t.Fatal(err)
	}

	if published == nil || len(published.GetFindings()) != 1 || published.GetTotal() != 1 || !recordedBefore {
		t.Fatalf("published %v (recorded first: %v)", published, recordedBefore)
	}

	published = nil
	if _, err := s.Run(t.Context(), cluster(nil)); err != nil { // due again: the clock did not move, Last is nil
		t.Fatal(err)
	}
	failing := scanner(t, &recorder{}, workload{err: errors.New("api down")})
	failing.Publish = func(context.Context, deprecations.Cluster, *cloudv1.ClusterDeprecations, *cloudv1.ClusterDeprecations) {
		t.Error("a failed scan was published")
	}
	if _, err := failing.Run(t.Context(), cluster(nil)); err != nil {
		t.Fatal(err)
	}
}
