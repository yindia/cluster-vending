package deprecation

import "testing"

// The report keeps at most max findings for the Cluster object and every
// one for the results bucket.
func TestSummarizeKeepsEveryFindingInAll(t *testing.T) {
	var findings []Finding
	for range 5000 {
		findings = append(findings, Finding{Removed: true})
	}

	r := Summarize("1.37", findings, MaxFindings)

	if r.Total != 5000 || r.Removed != 5000 || len(r.Findings) != MaxFindings || len(r.All) != 5000 || !r.Truncated() {
		t.Fatalf("total %d removed %d kept %d all %d", r.Total, r.Removed, len(r.Findings), len(r.All))
	}
}
