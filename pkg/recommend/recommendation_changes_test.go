package recommend_test

import (
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/recommend"
)

// ProtoChanges is Row.Changes for a recommendation read back from storage:
// the same rule, a tenth or more, or a request set where there is none.
func TestProtoChangesIsRowChanges(t *testing.T) {
	for name, row := range map[string]recommend.Row{
		"no recommendation": {Requests: recommend.Resources{CPUMillis: 100}},
		"same":              {Requests: recommend.Resources{CPUMillis: 100, MemoryBytes: 1000}, RecommendedRequests: recommend.Resources{CPUMillis: 100, MemoryBytes: 1000}},
		"a tenth":           {Requests: recommend.Resources{CPUMillis: 100}, RecommendedRequests: recommend.Resources{CPUMillis: 110}},
		"just under":        {Requests: recommend.Resources{CPUMillis: 100}, RecommendedRequests: recommend.Resources{CPUMillis: 109}},
		"no request":        {RecommendedRequests: recommend.Resources{MemoryBytes: 1 << 20}},
		"memory only":       {Requests: recommend.Resources{MemoryBytes: 1000}, RecommendedRequests: recommend.Resources{MemoryBytes: 2000}},
		"lower":             {Requests: recommend.Resources{CPUMillis: 1000}, RecommendedRequests: recommend.Resources{CPUMillis: 500}},
	} {
		if got, want := recommend.ProtoChanges(row.Proto()), row.Changes(); got != want {
			t.Errorf("%s: ProtoChanges = %v, Row.Changes = %v", name, got, want)
		}
	}
	if recommend.ProtoChanges(nil) || recommend.ProtoChanges(&cloudv1.ResourceRecommendation{}) {
		t.Error("an empty recommendation changes something")
	}
}
