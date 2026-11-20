package clustersummary

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func manyViolations(violations, events int) *cloudv1.ClusterPolicySnapshot {
	snap := &cloudv1.ClusterPolicySnapshot{}
	for i := 0; i < violations; i++ {
		snap.Violations = append(snap.Violations, &cloudv1.ClusterViolation{Severity: fmt.Sprintf("sev-%d", i%12)})
	}
	reasons := []string{"PolicyViolation", "FailedAdmission", "WarningAdmission", "DryrunViolation", "Other"}
	for i := 0; i < events; i++ {
		snap.Events = append(snap.Events, &cloudv1.ClusterViolationEvent{Reason: reasons[i%len(reasons)]})
	}
	return snap
}

func TestViolationCounts(t *testing.T) {
	now := time.Unix(10000, 0)
	snap := &cloudv1.ClusterPolicySnapshot{
		Summary: nil,
		Violations: []*cloudv1.ClusterViolation{
			{Severity: "high"}, {Severity: "high"}, {Severity: "low"}, {},
		},
		Events: []*cloudv1.ClusterViolationEvent{
			{Reason: "PolicyViolation", LastObservedAt: timestamppb.New(now.Add(-10 * time.Minute)), Count: 3},
			{Reason: "FailedAdmission", LastObservedAt: timestamppb.New(now.Add(-59 * time.Minute))},
			{Reason: "FailedAdmission", LastObservedAt: timestamppb.New(now.Add(-61 * time.Minute))}, // too old
			{Reason: "WarningAdmission"}, // no time: counted
			{Reason: "Mystery", LastObservedAt: timestamppb.New(now)},
		},
	}
	v, e := ViolationCounts(snap, now)
	if v.Total != 4 {
		t.Fatalf("violations total = %d", v.Total)
	}
	gotSev := map[string]int32{}
	for _, c := range v.BySeverity {
		gotSev[c.Name] = c.Count
	}
	if gotSev["high"] != 2 || gotSev["low"] != 1 || gotSev["unknown"] != 1 || len(gotSev) != 3 {
		t.Fatalf("severities = %v", gotSev)
	}
	if v.BySeverity[0].Name != "high" {
		t.Fatalf("not sorted by count: %v", v.BySeverity)
	}
	// 3 + 1 + 1 + 1 within the hour.
	if e.Total != 6 {
		t.Fatalf("events total = %d", e.Total)
	}
	gotReason := map[string]int32{}
	for _, c := range e.ByReason {
		gotReason[c.Name] = c.Count
	}
	if gotReason["PolicyViolation"] != 3 || gotReason["FailedAdmission"] != 1 || gotReason["WarningAdmission"] != 1 || gotReason["DryrunViolation"] != 0 || len(gotReason) != 3 {
		t.Fatalf("reasons = %v", gotReason)
	}
}

func TestViolationCountsOtherSeverity(t *testing.T) {
	v, _ := ViolationCounts(manyViolations(120, 0), time.Now())
	if len(v.BySeverity) != 9 {
		t.Fatalf("got %d entries, want 8 + other", len(v.BySeverity))
	}
	last := v.BySeverity[8]
	if last.Name != "other" || last.Count != 40 {
		t.Fatalf("other = %+v", last)
	}
	var sum int32
	for _, c := range v.BySeverity {
		sum += c.Count
	}
	if sum != v.Total || v.Total != 120 {
		t.Fatalf("sum %d total %d", sum, v.Total)
	}
}

func TestViolationCountsNil(t *testing.T) {
	v, e := ViolationCounts(nil, time.Now())
	if v.Total != 0 || e.Total != 0 || len(v.BySeverity) != 0 {
		t.Fatalf("got %+v %+v", v, e)
	}
}
