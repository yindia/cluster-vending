// Package policy collects bounded, read-only policy observations. It does not
// infer compliance, admission decisions, or counts from missing engine data.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	Interval = 5 * time.Minute
	Timeout  = 45 * time.Second
	EventTTL = time.Hour
	MaxBytes = 128 * 1024
	MaxRows  = 200
	// The full collection, for what is written to the results bucket, is
	// bounded by the object size (results.DefaultMaxBytes) and a per-pass
	// deadline, not by the Cluster object's caps: lists page 500 at a time.
	FullPageSize    = 500
	FullTimeout     = 2 * time.Minute
	fullPageLimit   = 400 // pages of one list: 200,000 items
	fullMaxRows     = 50000
	fullRequests    = 1000
	fullEventShare  = 100
	RetentionNotice = "Recent Kubernetes admission events only, not an audit history. Events may expire sooner than one hour or be disabled. Returned events are at most one hour old; absence does not prove no violations. Native VAP has no Kubernetes admission event stream."
)

func Empty(notice string) *cloudv1.ClusterPolicySnapshot {
	return &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{Incomplete: true, Notices: []string{notice}}}
}

// Stale keeps the last good observation when a refresh fails. Findings, events
// and collectedAt stay as they were; the snapshot is marked incomplete and says
// that the latest collection failed. With nothing worth keeping it is Empty.
func Stale(prev *cloudv1.ClusterPolicySnapshot, now time.Time, why string) *cloudv1.ClusterPolicySnapshot {
	if prev == nil || prev.Summary == nil || (prev.Summary.CollectedAt == nil && len(prev.Violations) == 0 && len(prev.Events) == 0) {
		return Empty(why)
	}
	Expire(prev, now)
	notice(prev, "The latest collection failed; showing the previous observation. "+why)
	return prev
}
func Decode(raw []byte) *cloudv1.ClusterPolicySnapshot {
	p := &cloudv1.ClusterPolicySnapshot{}
	if len(raw) == 0 {
		return Empty("Policy data has not been collected yet.")
	}
	if len(raw) > MaxBytes || (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, p) != nil || p.Summary == nil {
		return Empty("Policy snapshot is unreadable; collection is incomplete.")
	}
	return p
}
func notice(p *cloudv1.ClusterPolicySnapshot, s string) {
	p.Summary.Incomplete = true
	s = clip(s, 512)
	for _, n := range p.Summary.Notices {
		if n == s {
			return
		}
	}
	if len(p.Summary.Notices) < 32 {
		p.Summary.Notices = append(p.Summary.Notices, s)
	}
}
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
func identity(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:16])
}
func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
func Expire(p *cloudv1.ClusterPolicySnapshot, now time.Time) {
	events := p.Events[:0]
	for _, e := range p.Events {
		if e.LastObservedAt != nil && !e.LastObservedAt.AsTime().Before(now.Add(-EventTTL)) && !e.LastObservedAt.AsTime().After(now.Add(time.Minute)) {
			events = append(events, e)
		}
	}
	p.Events = events
}

// Encode enforces an aggregate bound as well as row bounds, including JSON
// escaping overhead. The two lists are independently bounded.
func Encode(p *cloudv1.ClusterPolicySnapshot) ([]byte, error) {
	for len(p.Violations) > MaxRows {
		p.Violations = p.Violations[:MaxRows]
		p.Summary.Truncated = true
	}
	for len(p.Events) > MaxRows {
		p.Events = p.Events[:MaxRows]
		p.Summary.Truncated = true
	}
	for {
		if p.Summary.Truncated {
			notice(p, "Snapshot limits reached; rows are incomplete.")
		}
		b, err := protojson.Marshal(p)
		if err != nil {
			return nil, err
		}
		if len(b) <= MaxBytes {
			return b, nil
		}
		p.Summary.Truncated = true
		switch {
		case len(p.Violations) > len(p.Events):
			p.Violations = p.Violations[:len(p.Violations)-1]
		case len(p.Events) > 0:
			p.Events = p.Events[:len(p.Events)-1]
		default:
			return nil, &sizeError{}
		}
	}
}

type sizeError struct{}

func (*sizeError) Error() string { return "policy snapshot metadata exceeds size limit" }
