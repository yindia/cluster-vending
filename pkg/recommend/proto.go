package recommend

import cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"

// sources are the contract's names of the sources.
var sources = map[Source]cloudv1.RecommendationSource{
	SourceVPA:          cloudv1.RecommendationSource_RECOMMENDATION_SOURCE_VPA,
	SourceUsage:        cloudv1.RecommendationSource_RECOMMENDATION_SOURCE_USAGE_ESTIMATE,
	SourceInsufficient: cloudv1.RecommendationSource_RECOMMENDATION_SOURCE_INSUFFICIENT_DATA,
	SourceNone:         cloudv1.RecommendationSource_RECOMMENDATION_SOURCE_NO_DATA,
}

func amounts(r Resources) *cloudv1.ResourceAmounts {
	if r == (Resources{}) {
		return nil
	}
	return &cloudv1.ResourceAmounts{CpuMillis: r.CPUMillis, MemoryBytes: r.MemoryBytes}
}

// Proto is the row as the contract has it.
func (r Row) Proto() *cloudv1.ResourceRecommendation {
	return &cloudv1.ResourceRecommendation{Namespace: r.Namespace, WorkloadKind: r.Kind, WorkloadName: r.Name,
		Container: r.Container, Replicas: r.Replicas, Platform: r.Platform,
		CurrentRequests: amounts(r.Requests), CurrentLimits: amounts(r.Limits),
		RecommendedRequests: amounts(r.RecommendedRequests), RecommendedLimits: amounts(r.RecommendedLimits),
		Difference: amounts(r.Difference), CpuPercent: r.CPUPercent, MemoryPercent: r.MemoryPercent,
		UsagePeak: amounts(r.Peak), UsageAverage: amounts(r.Average), Source: sources[r.Source],
		DataHours: r.DataHours, Samples: r.Samples}
}

// ProtoChanges is Row.Changes of a recommendation as it is stored or sent: it
// differs from what is set by a tenth or more, or sets a request where there
// is none. The API server filters a stored recommendation set with it.
func ProtoChanges(r *cloudv1.ResourceRecommendation) bool {
	rec := r.GetRecommendedRequests()
	if rec.GetCpuMillis() == 0 && rec.GetMemoryBytes() == 0 {
		return false
	}
	cur := r.GetCurrentRequests()
	return differs(cur.GetCpuMillis(), rec.GetCpuMillis()) || differs(cur.GetMemoryBytes(), rec.GetMemoryBytes())
}
