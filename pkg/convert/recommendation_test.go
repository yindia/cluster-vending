package convert_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/convert"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestRecommendationsRoundTrip(t *testing.T) {
	at := timestamppb.New(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	in := &cloudv1.ClusterRecommendations{SampledAt: at, AttemptedAt: at, Workloads: 3, Containers: 5, WindowDays: 7,
		MetricsAvailable: true, VpaAvailable: true, Truncated: true, Message: "fine"}

	out := convert.RecommendationsFromAPI(convert.RecommendationsToAPI(in))

	if !proto.Equal(in, out) {
		t.Fatalf("round trip = %v, want %v", out, in)
	}
	if convert.RecommendationsToAPI(nil) != nil || convert.RecommendationsFromAPI(nil) != nil {
		t.Error("nil is not nil")
	}
	long := convert.RecommendationsToAPI(&cloudv1.ClusterRecommendations{Error: strings.Repeat("x", 5000)})
	if len(long.Error) != 1024 {
		t.Errorf("error of %d bytes kept, want 1024", len(long.Error))
	}
}
