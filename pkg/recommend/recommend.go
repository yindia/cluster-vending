// Package recommend works out resource requests for the workloads of a
// workload cluster from what the cluster already reports, without
// installing anything in it: the workloads' own requests and limits, the
// usage metrics-server serves (metrics.k8s.io) and, where the tenant runs
// the Vertical Pod Autoscaler, its recommendations.
//
// It is pure: a Reader reads the cluster (KubeReader is the real one), a
// History keeps a bounded rolling record of usage per container, and Rows
// turns a History into recommendations. Nothing here writes to a cluster.
package recommend

import (
	"cmp"
	"slices"
	"time"
)

// Resources are CPU in millicores and memory in bytes; zero is "none".
type Resources struct {
	CPUMillis   int64 `json:"cpu,omitempty"`
	MemoryBytes int64 `json:"mem,omitempty"`
}

// Container is a container of a workload with the requests and limits its
// pod template sets.
type Container struct {
	Name     string
	Requests Resources
	Limits   Resources
}

// Workload is a Deployment, StatefulSet or DaemonSet.
type Workload struct {
	Namespace, Kind, Name string
	Replicas              int32
	Containers            []Container
}

// VPARecommendation is what a Vertical Pod Autoscaler recommends for one
// container.
type VPARecommendation struct {
	Target     Resources `json:"target"`
	UpperBound Resources `json:"upper,omitempty"`
}

// Key names one container of one workload.
type Key struct{ Namespace, Kind, Name, Container string }

// Source is where a row's recommendation comes from.
type Source string

const (
	// SourceVPA: the cluster's own Vertical Pod Autoscaler recommends it.
	SourceVPA Source = "vpa"
	// SourceUsage: estimated from the usage recorded over the window.
	SourceUsage Source = "usage-estimate"
	// SourceInsufficient: usage is recorded, but for less than MinData.
	SourceInsufficient Source = "insufficient-data"
	// SourceNone: no usage was ever recorded and no VPA recommends.
	SourceNone Source = "no-data"
)

// The rules of a usage estimate. They are deliberately simple:
//
//   - request = the peak over the window with a margin of 15%, rounded up
//     (CPU to 10m, memory to 1Mi), never below a floor (10m, 16Mi);
//   - memory limit = the recommended memory request plus 20%, rounded up
//     to 1Mi: memory over the limit kills the container, so the limit
//     stays a step above what was seen;
//   - no CPU limit is recommended: CPU over a limit only throttles, and
//     the request already guarantees the container its share.
//
// A Vertical Pod Autoscaler's recommendation wins over the estimate: its
// target is the request, and its upper bound the memory limit.
const (
	// Window is how far back usage is kept.
	Window = 7 * 24 * time.Hour
	// MinData is how long usage must have been recorded before an estimate
	// is made from it.
	MinData = 24 * time.Hour

	marginPercent      = 115
	limitPercent       = 120
	cpuStepMillis      = 10
	memoryStepBytes    = 1 << 20
	floorCPUMillis     = 10
	floorMemoryBytes   = 16 << 20
	changePercent      = 10
	platformMemoryBias = 4 << 20 // score: one millicore weighs as 4 MiB
)

// Row is one container with what it asks for now and what is recommended.
type Row struct {
	Namespace, Kind, Name, Container string
	Replicas                         int32
	// Platform: the namespace is one of the platform's components'.
	Platform bool
	// Requests and Limits are what the workload sets now.
	Requests, Limits Resources
	// RecommendedRequests and RecommendedLimits are zero where nothing is
	// recommended (always for the CPU limit).
	RecommendedRequests, RecommendedLimits Resources
	// Difference is recommended minus current requests, for the resources
	// that have a recommendation; the percentages are of the current
	// request and zero when there is none.
	Difference                Resources
	CPUPercent, MemoryPercent int32
	// Peak and Average are the usage over the window.
	Peak, Average Resources
	Source        Source
	// DataHours is how long usage has been recorded; Samples how often.
	DataHours int32
	Samples   int32
}

// Recommended reports whether the row carries a recommendation.
func (r Row) Recommended() bool {
	return r.RecommendedRequests != (Resources{})
}

// Changes reports whether the recommendation differs from what is set by
// a tenth or more (or sets a request where there is none).
func (r Row) Changes() bool {
	if !r.Recommended() {
		return false
	}
	return differs(r.Requests.CPUMillis, r.RecommendedRequests.CPUMillis) ||
		differs(r.Requests.MemoryBytes, r.RecommendedRequests.MemoryBytes)
}

func differs(current, recommended int64) bool {
	if recommended == 0 {
		return false
	}
	if current == 0 {
		return true
	}
	diff := recommended - current
	if diff < 0 {
		diff = -diff
	}
	return diff*100 >= current*changePercent
}

// Rows are the recommendations a history gives at now, ordered by
// namespace, kind, workload and container.
func Rows(h History, now time.Time) []Row {
	var out []Row
	for _, w := range h.Workloads {
		for _, c := range w.Containers {
			out = append(out, row(w, c, now))
		}
	}
	slices.SortFunc(out, func(a, b Row) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Name, b.Name), cmp.Compare(a.Container, b.Container))
	})
	return out
}

func row(w WorkloadRecord, c ContainerRecord, now time.Time) Row {
	r := Row{Namespace: w.Namespace, Kind: w.Kind, Name: w.Name, Container: c.Name, Replicas: w.Replicas,
		Platform: w.Platform, Requests: c.Requests, Limits: c.Limits, Source: SourceNone,
		Samples: int32(min(c.Samples, 1<<31-1))} //nolint:gosec // bounded above
	peak, sum, n := c.usage(now)
	r.Peak = peak
	if n > 0 {
		r.Average = Resources{CPUMillis: sum.CPUMillis / n, MemoryBytes: sum.MemoryBytes / n}
	}
	recorded := time.Duration(c.Last-c.First) * time.Second
	if c.Samples > 0 {
		r.DataHours = int32(recorded / time.Hour) //nolint:gosec // at most the window
	}
	switch {
	case c.VPA != nil && c.VPA.Target != (Resources{}):
		r.Source = SourceVPA
		r.RecommendedRequests = Resources{CPUMillis: floorTo(c.VPA.Target.CPUMillis, floorCPUMillis, c.VPA.Target.CPUMillis != 0),
			MemoryBytes: floorTo(c.VPA.Target.MemoryBytes, floorMemoryBytes, c.VPA.Target.MemoryBytes != 0)}
		if upper := c.VPA.UpperBound.MemoryBytes; upper > 0 {
			r.RecommendedLimits.MemoryBytes = max(roundUp(upper, memoryStepBytes), r.RecommendedRequests.MemoryBytes)
		}
	case n == 0:
		// Nothing recorded in the window.
	case recorded < MinData:
		r.Source = SourceInsufficient
	default:
		r.Source = SourceUsage
		r.RecommendedRequests = Resources{
			CPUMillis:   max(roundUp(peak.CPUMillis*marginPercent/100, cpuStepMillis), floorCPUMillis),
			MemoryBytes: max(roundUp(peak.MemoryBytes*marginPercent/100, memoryStepBytes), floorMemoryBytes),
		}
		r.RecommendedLimits.MemoryBytes = roundUp(r.RecommendedRequests.MemoryBytes*limitPercent/100, memoryStepBytes)
	}
	if rec := r.RecommendedRequests; rec != (Resources{}) {
		if rec.CPUMillis != 0 {
			r.Difference.CPUMillis = rec.CPUMillis - r.Requests.CPUMillis
			r.CPUPercent = percent(r.Difference.CPUMillis, r.Requests.CPUMillis)
		}
		if rec.MemoryBytes != 0 {
			r.Difference.MemoryBytes = rec.MemoryBytes - r.Requests.MemoryBytes
			r.MemoryPercent = percent(r.Difference.MemoryBytes, r.Requests.MemoryBytes)
		}
	}
	return r
}

// floorTo is v raised to floor when set is true, else 0.
func floorTo(v, floor int64, set bool) int64 {
	if !set {
		return 0
	}
	return max(v, floor)
}

// roundUp is v rounded up to a multiple of step.
func roundUp(v, step int64) int64 {
	if v <= 0 {
		return 0
	}
	return (v + step - 1) / step * step
}

// percent is diff as a percentage of current, rounded towards zero; 0
// without a current value.
func percent(diff, current int64) int32 {
	if current == 0 {
		return 0
	}
	p := diff * 100 / current
	return int32(max(min(p, 1<<31-1), -(1 << 31)))
}
