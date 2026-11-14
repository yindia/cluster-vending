package recommendations_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/recommend"
	"github.com/yindia/cluster-vending/pkg/reconcile/recommendations"
)

var t0 = time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)

// store is the cluster's recorded usage and summary.
type store struct {
	history []byte
	summary *cloudv1.ClusterRecommendations
	failSet error
	writes  int
}

func (s *store) UsageHistory(context.Context, string, string) ([]byte, error) { return s.history, nil }
func (s *store) SetUsageHistory(_ context.Context, _, _ string, h []byte) error {
	s.writes++
	s.history = h
	return s.failSet
}
func (s *store) SetRecommendations(_ context.Context, _, _ string, raw model.JSON) error {
	s.summary = &cloudv1.ClusterRecommendations{}
	return protojson.Unmarshal(raw, s.summary)
}

// cluster is a workload cluster as a recommend.Reader sees it.
type cluster struct {
	metrics, vpa bool
	failWork     error
}

func (c cluster) Workloads(context.Context) ([]recommend.Workload, error) {
	return []recommend.Workload{{Namespace: "shop", Kind: "Deployment", Name: "web", Replicas: 1,
		Containers: []recommend.Container{{Name: "app"}}},
		{Namespace: "kyverno", Kind: "Deployment", Name: "admission", Replicas: 1,
			Containers: []recommend.Container{{Name: "kyverno"}}}}, c.failWork
}
func (c cluster) Pods(context.Context) ([]recommend.Pod, error) {
	return []recommend.Pod{{Namespace: "shop", Name: "web-1-a", OwnerKind: "ReplicaSet", OwnerName: "web-1"}}, nil
}
func (c cluster) PodMetrics(context.Context) ([]recommend.PodUsage, bool, error) {
	if !c.metrics {
		return nil, false, nil
	}
	return []recommend.PodUsage{{Namespace: "shop", Pod: "web-1-a",
		Containers: map[string]recommend.Resources{"app": {CPUMillis: 50, MemoryBytes: 64 << 20}}}}, true, nil
}
func (c cluster) VPAs(context.Context) ([]recommend.VPA, bool, error) { return nil, c.vpa, nil }

func sampler(s *store, c cluster, at time.Time) recommendations.Sampler {
	return recommendations.Sampler{Store: s, Now: func() time.Time { return at },
		Connect: func([]byte) (recommend.Reader, error) { return c, nil }}
}

func target(last *cloudv1.ClusterRecommendations) recommendations.Cluster {
	return recommendations.Cluster{TenantID: "acme", ID: "c1", Last: last,
		Kubeconfig: func(context.Context) ([]byte, error) { return []byte("kubeconfig"), nil },
		Platform:   func(ns string) bool { return ns == "kyverno" }}
}

func TestSampleRecordsUsageAndTheSummary(t *testing.T) {
	s := &store{}

	next, err := sampler(s, cluster{metrics: true}, t0).Run(t.Context(), target(nil))

	if err != nil || next != recommendations.Interval {
		t.Fatalf("Run = %s, %v", next, err)
	}
	h, err := recommend.Decode(s.history)
	if err != nil || h.Containers() != 2 || !h.Metrics {
		t.Fatalf("history = %+v, %v", h, err)
	}
	if !h.Workloads[0].Platform && !h.Workloads[1].Platform {
		t.Error("kyverno's namespace is not marked the platform's")
	}
	sum := s.summary
	if sum.GetContainers() != 2 || sum.GetWorkloads() != 2 || sum.GetWindowDays() != 7 || !sum.GetMetricsAvailable() ||
		!sum.GetSampledAt().AsTime().Equal(t0) || sum.GetError() != "" {
		t.Errorf("summary = %v", sum)
	}
	if !strings.Contains(sum.GetMessage(), "first recommendations after a day") {
		t.Errorf("message = %q", sum.GetMessage())
	}
}

func TestSampleKeepsWhatWasRecorded(t *testing.T) {
	s := &store{}
	first := sampler(s, cluster{metrics: true}, t0)
	if _, err := first.Run(t.Context(), target(nil)); err != nil {
		t.Fatal(err)
	}
	last := s.summary

	if _, err := sampler(s, cluster{metrics: true}, t0.Add(recommendations.Interval)).Run(t.Context(), target(last)); err != nil {
		t.Fatal(err)
	}

	h, _ := recommend.Decode(s.history)
	for _, w := range h.Workloads {
		if w.Name == "web" && w.Containers[0].Samples != 2 {
			t.Errorf("web has %d samples, want 2", w.Containers[0].Samples)
		}
	}
}

func TestSampleIsNotDueYet(t *testing.T) {
	s := &store{}
	last := &cloudv1.ClusterRecommendations{AttemptedAt: timestamppb.New(t0)}

	next, err := sampler(s, cluster{metrics: true}, t0.Add(time.Minute)).Run(t.Context(), target(last))

	if err != nil || next != recommendations.Interval-time.Minute || s.writes != 0 {
		t.Fatalf("Run = %s, %v, %d writes; want to wait 4m", next, err, s.writes)
	}
}

func TestSampleWithoutMetricsOrVPA(t *testing.T) {
	s := &store{}

	if _, err := sampler(s, cluster{}, t0).Run(t.Context(), target(nil)); err != nil {
		t.Fatal(err)
	}

	if msg := s.summary.GetMessage(); !strings.Contains(msg, "neither metrics-server") {
		t.Errorf("message = %q", msg)
	}
}

func TestSampleThatFailsIsRecorded(t *testing.T) {
	last := &cloudv1.ClusterRecommendations{SampledAt: timestamppb.New(t0.Add(-time.Hour)), Containers: 9,
		AttemptedAt: timestamppb.New(t0.Add(-time.Hour)), MetricsAvailable: true}
	tests := []struct {
		name string
		c    cluster
		kube func(context.Context) ([]byte, error)
		want string
	}{
		{"workloads cannot be listed", cluster{failWork: errors.New("forbidden")}, nil, "forbidden"},
		{"no kubeconfig", cluster{}, func(context.Context) ([]byte, error) { return nil, errors.New("no kubeconfig Secret yet") },
			"no kubeconfig Secret yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &store{}
			c := target(last)
			if tt.kube != nil {
				c.Kubeconfig = tt.kube
			}

			next, err := sampler(s, tt.c, t0).Run(t.Context(), c)

			if err != nil || next != recommendations.RetryInterval || s.writes != 0 {
				t.Fatalf("Run = %s, %v, %d writes", next, err, s.writes)
			}
			if !strings.Contains(s.summary.GetError(), tt.want) || s.summary.GetContainers() != 9 ||
				!s.summary.GetSampledAt().AsTime().Equal(t0.Add(-time.Hour)) || !s.summary.GetAttemptedAt().AsTime().Equal(t0) {
				t.Errorf("summary = %v; want the error, and the last sample's counts", s.summary)
			}
		})
	}
}

// History this build cannot read (another version) is started again, and
// the summary says so.
func TestSampleStartsOverOnUnreadableHistory(t *testing.T) {
	s := &store{history: []byte("not gzip")}

	if _, err := sampler(s, cluster{metrics: true}, t0).Run(t.Context(), target(nil)); err != nil {
		t.Fatal(err)
	}

	if h, err := recommend.Decode(s.history); err != nil || h.Containers() != 2 {
		t.Fatalf("history = %+v, %v", h, err)
	}
	if !strings.Contains(s.summary.GetMessage(), "started again") {
		t.Errorf("message = %q", s.summary.GetMessage())
	}
}
