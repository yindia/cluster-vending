package convert

import (
	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// maxRecommendationText is the longest error or message the kind holds.
const maxRecommendationText = 1024

// RecommendationsToAPI is the kind's view of a recommendations summary;
// nil for none.
func RecommendationsToAPI(p *cloudv1.ClusterRecommendations) *cloudv1alpha1.ClusterRecommendations {
	if p == nil {
		return nil
	}
	return &cloudv1alpha1.ClusterRecommendations{SampledAt: timeOf(p.GetSampledAt()), AttemptedAt: timeOf(p.GetAttemptedAt()),
		Workloads: p.GetWorkloads(), Containers: p.GetContainers(), WindowDays: p.GetWindowDays(),
		MetricsAvailable: p.GetMetricsAvailable(), VPAAvailable: p.GetVpaAvailable(), Truncated: p.GetTruncated(),
		Error: cutText(p.GetError()), Message: cutText(p.GetMessage())}
}

// RecommendationsFromAPI is the contract's view of what the kind holds;
// nil for none.
func RecommendationsFromAPI(a *cloudv1alpha1.ClusterRecommendations) *cloudv1.ClusterRecommendations {
	if a == nil {
		return nil
	}
	return &cloudv1.ClusterRecommendations{SampledAt: timestampOf(a.SampledAt), AttemptedAt: timestampOf(a.AttemptedAt),
		Workloads: a.Workloads, Containers: a.Containers, WindowDays: a.WindowDays,
		MetricsAvailable: a.MetricsAvailable, VpaAvailable: a.VPAAvailable, Truncated: a.Truncated,
		Error: a.Error, Message: a.Message}
}

func cutText(s string) string {
	if len(s) > maxRecommendationText {
		return s[:maxRecommendationText]
	}
	return s
}
