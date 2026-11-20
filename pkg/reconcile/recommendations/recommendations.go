// Package recommendations samples a workload cluster for resource
// recommendations (pkg/recommend): its workloads, the usage metrics-server
// reports and the cluster's own VPA recommendations, read-only. It keeps
// the rolling usage history in the management cluster and a summary on
// the cluster's row. It is never part of the cluster's own reconcile: a
// sample that fails is recorded and tried again.
package recommendations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/deprecation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/recommend"
)

const (
	// Interval is how often a cluster is sampled.
	Interval = 5 * time.Minute
	// RetryInterval is how long until a sample that failed is tried again.
	RetryInterval = 15 * time.Minute
	// maxText is the longest error or message a summary records.
	maxText = 512
)

// Store keeps a cluster's recorded usage and its summary
// (repository.Clusters).
type Store interface {
	UsageHistory(ctx context.Context, tenantID, id string) ([]byte, error)
	SetUsageHistory(ctx context.Context, tenantID, id string, history []byte) error
	SetRecommendations(ctx context.Context, tenantID, id string, recommendations model.JSON) error
}

// Cluster is what a sample needs to know of a cluster.
type Cluster struct {
	TenantID, ID string
	// Last is the stored summary of the last sample; nil for none.
	Last *cloudv1.ClusterRecommendations
	// Kubeconfig reads the cluster's kubeconfig, when a sample is due.
	Kubeconfig func(context.Context) ([]byte, error)
	// Platform says which namespaces are the platform components'.
	Platform func(namespace string) bool
}

// Sampler samples clusters, one at a time.
type Sampler struct {
	Store Store
	// Connect opens a workload cluster from its kubeconfig; nil uses
	// client-go with deprecation.RESTConfig's refusals.
	Connect func(kubeconfig []byte) (recommend.Reader, error)
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// Publish is given a sample that worked, before its summary is recorded:
	// the controller computes the recommendations from the recorded usage and
	// writes them to the cluster's results bucket. What it returns, when not
	// empty, is a notice added to the summary's message (the results are in
	// the Cluster object's reach only). It cannot fail the sample. Nil
	// publishes nothing.
	Publish func(ctx context.Context, c Cluster, summary *cloudv1.ClusterRecommendations, h recommend.History, at time.Time) string
}

func (s Sampler) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s Sampler) connect(kubeconfig []byte) (recommend.Reader, error) {
	if s.Connect != nil {
		return s.Connect(kubeconfig)
	}
	cfg, err := deprecation.RESTConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return recommend.NewKubeReader(cfg)
}

// due says whether the cluster is to be sampled now, and else how long
// until it is.
func (s Sampler) due(last *cloudv1.ClusterRecommendations) (bool, time.Duration) {
	if last == nil || last.GetAttemptedAt() == nil {
		return true, 0
	}
	stands := Interval
	if last.GetError() != "" {
		stands = RetryInterval
	}
	if wait := stands - s.now().Sub(last.GetAttemptedAt().AsTime()); wait > 0 {
		return false, wait
	}
	return true, 0
}

// Run samples the cluster when a sample is due and records the result,
// and returns how long until the next one. An error is only a result that
// could not be recorded: a sample that fails is itself recorded, with what
// the last sample that worked found.
func (s Sampler) Run(ctx context.Context, c Cluster) (time.Duration, error) {
	if due, wait := s.due(c.Last); !due {
		return wait, nil
	}
	at := s.now()
	summary, next := &cloudv1.ClusterRecommendations{AttemptedAt: timestamppb.New(at)}, Interval
	h, notes, err := s.sample(ctx, c, at)
	switch {
	case err != nil && errors.Is(ctx.Err(), context.Canceled):
		return 0, ctx.Err() // shutting down: say nothing about the cluster
	case err != nil:
		if last := c.Last; last != nil {
			summary.SampledAt, summary.Workloads, summary.Containers = last.GetSampledAt(), last.GetWorkloads(), last.GetContainers()
			summary.WindowDays, summary.Truncated = last.GetWindowDays(), last.GetTruncated()
			summary.MetricsAvailable, summary.VpaAvailable = last.GetMetricsAvailable(), last.GetVpaAvailable()
		}
		summary.Error, next = cut(err.Error()), RetryInterval
	default:
		summary.SampledAt = timestamppb.New(at)
		summary.Workloads, summary.Containers = int32(len(h.Workloads)), int32(h.Containers()) //nolint:gosec // bounded by recommend.MaxContainers
		summary.WindowDays = int32(recommend.Window / (24 * time.Hour))
		summary.MetricsAvailable, summary.VpaAvailable, summary.Truncated = h.Metrics, h.VPA, h.Truncated
		summary.Message = message(h, notes, at)
		if s.Publish != nil {
			if notice := s.Publish(ctx, c, summary, h, at); notice != "" {
				summary.Message = strings.TrimSpace(summary.Message + " " + notice)
			}
		}
		summary.Message = cut(summary.Message)
	}
	raw, err := protojson.Marshal(summary)
	if err != nil {
		return 0, err
	}
	if err := s.Store.SetRecommendations(ctx, c.TenantID, c.ID, raw); err != nil {
		return 0, fmt.Errorf("record the recommendations summary: %w", err)
	}
	return next, nil
}

// sample reads the cluster, adds what it read to the recorded usage and
// stores it.
func (s Sampler) sample(ctx context.Context, c Cluster, at time.Time) (recommend.History, []string, error) {
	kubeconfig, err := c.Kubeconfig(ctx)
	if err != nil {
		return recommend.History{}, nil, fmt.Errorf("kubeconfig: %w", err)
	}
	reader, err := s.connect(kubeconfig)
	if err != nil {
		return recommend.History{}, nil, err
	}
	snapshot, err := recommend.Sample(ctx, reader, at)
	if err != nil {
		return recommend.History{}, nil, err
	}
	notes := snapshot.Notes
	raw, err := s.Store.UsageHistory(ctx, c.TenantID, c.ID)
	if err != nil {
		return recommend.History{}, nil, fmt.Errorf("read the recorded usage: %w", err)
	}
	before, err := recommend.Decode(raw)
	if err != nil {
		before, notes = recommend.History{}, append(notes, "the recorded usage could not be read and was started again: "+err.Error())
	}
	encoded, h, err := before.Add(snapshot, c.Platform).Encode()
	if err != nil {
		return recommend.History{}, nil, err
	}
	if err := s.Store.SetUsageHistory(ctx, c.TenantID, c.ID, encoded); err != nil {
		return recommend.History{}, nil, fmt.Errorf("store the recorded usage: %w", err)
	}
	return h, notes, nil
}

// message says what stands in the way of recommendations: "" when nothing
// does.
func message(h recommend.History, notes []string, at time.Time) string {
	parts := notes
	switch {
	case !h.Metrics && !h.VPA:
		parts = append(parts, "the cluster has neither metrics-server (the metrics API) nor a Vertical Pod Autoscaler: "+
			"no usage can be recorded, so nothing is recommended")
	case h.Metrics && recordedFor(h, at) < recommend.MinData:
		parts = append(parts, "recording usage: first recommendations after a day, reliable after a week")
	}
	if h.Truncated {
		parts = append(parts, fmt.Sprintf("%d containers are not recorded (only the %d largest consumers are)", h.Dropped, recommend.MaxContainers))
	}
	return strings.Join(parts, "; ")
}

// recordedFor is the longest any container's usage has been recorded.
func recordedFor(h recommend.History, at time.Time) time.Duration {
	var longest time.Duration
	for _, w := range h.Workloads {
		for _, c := range w.Containers {
			if c.Samples > 0 {
				longest = max(longest, at.Sub(time.Unix(c.First, 0)))
			}
		}
	}
	return longest
}

func cut(s string) string {
	if len(s) > maxText {
		return s[:maxText]
	}
	return s
}
