package convert

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The kind reports a catalog change's phase, progress and times; the
// applied fingerprints stay in contractStatus, and the pending list is
// bounded.
func TestCatalogChangeToAPI(t *testing.T) {
	at := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	many := make([]string, 0, maxCatalogPending+5)
	for i := range maxCatalogPending + 5 {
		many = append(many, fmt.Sprintf("c%d", i))
	}
	tests := []struct {
		name    string
		in      *cloudv1.CatalogChange
		phase   string
		pending int
	}{
		{"unset", nil, "", 0},
		{"applying", &cloudv1.CatalogChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING, Component: "kyverno", Step: 1, Steps: 2,
			Pending: []string{"kyverno", "istiod"}, StartTime: timestamppb.New(at), Applied: map[string]string{"kyverno": "a"}}, CodeChangeApplying, 2},
		{"a long pending list is cut", &cloudv1.CatalogChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING, Pending: many}, CodeChangePending, maxCatalogPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CatalogChangeToAPI(tt.in)
			if tt.in == nil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got.Phase != tt.phase || len(got.Pending) != tt.pending || got.Component != tt.in.GetComponent() || got.Steps != tt.in.GetSteps() {
				t.Errorf("got %+v", got)
			}
			if tt.in.GetStartTime() != nil && !got.StartTime.Time.Equal(at) {
				t.Errorf("start = %v", got.StartTime)
			}
		})
	}
}
