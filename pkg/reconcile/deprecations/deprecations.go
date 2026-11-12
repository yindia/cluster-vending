// Package deprecations scans a workload cluster for objects written with
// deprecated or removed API versions (pkg/deprecation) and records what
// it found on the cluster's row. It is read-only on the workload cluster
// and never part of the cluster's own reconcile: a scan that fails is
// recorded and tried again, the cluster is not touched.
package deprecations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/deprecation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	// Interval is how long a scan that worked stands.
	Interval = 6 * time.Hour
	// RetryInterval is how long until a scan that failed is tried again.
	RetryInterval = 15 * time.Minute
	// maxError is the longest error a scan records.
	maxError = 512
)

// Recorder stores a scan result on a cluster's row
// (repository.Clusters).
type Recorder interface {
	SetDeprecations(ctx context.Context, tenantID, id string, deprecations model.JSON) error
}

// Cluster is what a scan needs to know of a cluster.
type Cluster struct {
	TenantID, ID string
	// Current is the Kubernetes version the control plane runs; UpgradeTo
	// the minor its release moves that version to ("" for none).
	Current, UpgradeTo string
	// Last is the stored result of the last scan; nil for none.
	Last *cloudv1.ClusterDeprecations
	// Kubeconfig reads the cluster's kubeconfig, when a scan is due.
	Kubeconfig func(context.Context) ([]byte, error)
}

// Scanner scans clusters, one at a time.
type Scanner struct {
	Rules    deprecation.RuleSet
	Recorder Recorder
	// Connect opens a workload cluster from its kubeconfig; nil uses
	// client-go (deprecation.NewKubeReader).
	Connect func(kubeconfig []byte) (deprecation.Reader, error)
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

func (s Scanner) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s Scanner) connect(kubeconfig []byte) (deprecation.Reader, error) {
	if s.Connect != nil {
		return s.Connect(kubeconfig)
	}
	return deprecation.NewKubeReader(kubeconfig)
}

// Due says whether the cluster is to be scanned now, and else how long
// until it is: a scan stands for Interval (a failed one for
// RetryInterval), and not at all once the target version changed.
func (s Scanner) Due(last *cloudv1.ClusterDeprecations, target string) (bool, time.Duration) {
	if last == nil || last.GetAttemptedAt() == nil || last.GetTargetVersion() != target {
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

// Run scans the cluster when a scan is due and records the result. It
// returns how long until the next scan. An error is only a result that
// could not be recorded: a scan that fails is itself recorded (with what
// the last scan that worked found) and is not an error.
func (s Scanner) Run(ctx context.Context, c Cluster) (time.Duration, error) {
	target, err := deprecation.Target(c.Current, c.UpgradeTo)
	if err != nil {
		// No version yet: nothing to judge against. Not worth recording.
		return RetryInterval, nil //nolint:nilerr // not a failure of the scan: there is nothing to scan for yet
	}
	if due, wait := s.Due(c.Last, target); !due {
		return wait, nil
	}
	now := timestamppb.New(s.now())
	result, next := &cloudv1.ClusterDeprecations{TargetVersion: target, AttemptedAt: now}, Interval
	report, err := s.scan(ctx, c, target)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return 0, ctx.Err() // shutting down: say nothing about the cluster
		}
		// Keep what the last scan that worked found; say why this one did not.
		if c.Last != nil {
			result.ScannedAt, result.Total, result.Removed = c.Last.GetScannedAt(), c.Last.GetTotal(), c.Last.GetRemoved()
			result.Findings, result.Truncated = c.Last.GetFindings(), c.Last.GetTruncated()
			if c.Last.GetTargetVersion() != target {
				// Judged against another version: the counts no longer hold.
				result.ScannedAt, result.Total, result.Removed, result.Findings, result.Truncated = nil, 0, 0, nil, false
			}
		}
		result.Error, next = cut(err.Error()), RetryInterval
	} else {
		result.ScannedAt, result.Total, result.Removed = now, int32(report.Total), int32(report.Removed) //nolint:gosec // counts of objects in one cluster
		result.Truncated = report.Truncated()
		for _, f := range report.Findings {
			result.Findings = append(result.Findings, convert.DeprecationFinding(f))
		}
	}
	raw, err := protojson.Marshal(result)
	if err != nil {
		return 0, err
	}
	if err := s.Recorder.SetDeprecations(ctx, c.TenantID, c.ID, raw); err != nil {
		return 0, fmt.Errorf("record the deprecation scan: %w", err)
	}
	return next, nil
}

func (s Scanner) scan(ctx context.Context, c Cluster, target string) (deprecation.Report, error) {
	kubeconfig, err := c.Kubeconfig(ctx)
	if err != nil {
		return deprecation.Report{}, fmt.Errorf("kubeconfig: %w", err)
	}
	reader, err := s.connect(kubeconfig)
	if err != nil {
		return deprecation.Report{}, err
	}
	return s.Rules.Scan(ctx, reader, target)
}

func cut(s string) string {
	if len(s) > maxError {
		return s[:maxError]
	}
	return s
}
