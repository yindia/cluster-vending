package clustersummary

import (
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	// eventWindow is how far back events count: Kubernetes keeps them about an
	// hour.
	eventWindow = time.Hour
	// maxSeverities are the severities kept by name; the rest are "other".
	maxSeverities = 8
)

// eventReasons are the reasons events are counted under.
var eventReasons = []string{"PolicyViolation", "FailedAdmission", "WarningAdmission", "DryrunViolation"}

// ViolationCounts are the counts a policy snapshot gives: violations by
// severity (the 8 largest by name, the rest under "other") and the events of
// the last hour, in total and under the four reasons. Events without an
// observation time count. It keeps no rows. A nil snapshot has no counts.
func ViolationCounts(snap *cloudv1.ClusterPolicySnapshot, now time.Time) (*cloudv1alpha1.ClusterViolationsStatus, *cloudv1alpha1.ClusterViolationEventsStatus) {
	at := metav1.NewTime(now.UTC())
	v := &cloudv1alpha1.ClusterViolationsStatus{PolicyCollectedAt: &at}
	e := &cloudv1alpha1.ClusterViolationEventsStatus{PolicyCollectedAt: &at}
	bySeverity := map[string]int32{}
	for _, x := range snap.GetViolations() {
		sev := strings.ToLower(strings.TrimSpace(x.GetSeverity()))
		if sev == "" {
			sev = cloudv1alpha1.StatusUnknownSeverityLabel
		}
		bySeverity[sev]++
		v.Total++
	}
	v.BySeverity = topCounts(bySeverity)
	byReason := map[string]int32{}
	for _, x := range snap.GetEvents() {
		if t := x.GetLastObservedAt(); t != nil && now.Sub(t.AsTime()) > eventWindow {
			continue
		}
		n := max(x.GetCount(), 1)
		e.Total += n
		if slices.Contains(eventReasons, x.GetReason()) {
			byReason[x.GetReason()] += n
		}
	}
	for _, r := range eventReasons {
		if byReason[r] > 0 {
			e.ByReason = append(e.ByReason, cloudv1alpha1.ClusterCount{Name: r, Count: byReason[r]})
		}
	}
	return v, e
}

// topCounts are the largest counts by name, then the rest summed as "other".
func topCounts(m map[string]int32) []cloudv1alpha1.ClusterCount {
	out := make([]cloudv1alpha1.ClusterCount, 0, len(m))
	for n, c := range m {
		out = append(out, cloudv1alpha1.ClusterCount{Name: n, Count: c})
	}
	slices.SortFunc(out, func(a, b cloudv1alpha1.ClusterCount) int {
		if a.Count != b.Count {
			return int(b.Count - a.Count)
		}
		return strings.Compare(a.Name, b.Name)
	})
	if len(out) <= maxSeverities {
		return out
	}
	var rest int32
	for _, c := range out[maxSeverities:] {
		rest += c.Count
	}
	return append(out[:maxSeverities:maxSeverities], cloudv1alpha1.ClusterCount{Name: cloudv1alpha1.StatusOtherSeverity, Count: rest})
}
