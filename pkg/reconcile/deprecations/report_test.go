package deprecations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/reconcile/deprecations"
)

// A scan reports what its result means once it is recorded, and nothing
// when the next one finds the same.
func TestRunReportsWhatChanged(t *testing.T) {
	rec := &recorder{}
	var reported []deprecations.Event
	s := scanner(t, rec, workload{})
	s.Report = func(_ context.Context, c deprecations.Cluster, e deprecations.Event) {
		if c.ID != "c1" {
			t.Errorf("reported for %q", c.ID)
		}
		reported = append(reported, e)
	}

	if _, err := s.Run(t.Context(), cluster(nil)); err != nil {
		t.Fatal(err)
	}

	if len(reported) != 1 || reported[0].Type != "Warning" || reported[0].Reason != deprecations.ReasonDeprecatedAPIs {
		t.Fatalf("reported %+v, want one DeprecatedAPIs warning", reported)
	}
	// The same findings again (a scan is due: the last one is old).
	last := rec.got
	last.AttemptedAt, last.ScannedAt = ago(7*time.Hour), ago(7*time.Hour)
	if _, err := s.Run(t.Context(), cluster(last)); err != nil {
		t.Fatal(err)
	}
	if len(reported) != 1 {
		t.Errorf("the same result was reported again: %+v", reported)
	}
}

// What cannot be recorded is not reported.
func TestRunDoesNotReportWhatIsNotRecorded(t *testing.T) {
	rec := &recorder{err: errors.New("the API server is down")}
	s := scanner(t, rec, workload{})
	s.Report = func(context.Context, deprecations.Cluster, deprecations.Event) { t.Error("reported") }

	if _, err := s.Run(t.Context(), cluster(nil)); err == nil {
		t.Fatal("Run: no error")
	}
}
