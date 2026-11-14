package recommend_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/recommend"
)

const (
	mi = int64(1) << 20
	gi = int64(1) << 30
)

var start = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func web(requests, limits recommend.Resources) recommend.Workload {
	return recommend.Workload{Namespace: "shop", Kind: "Deployment", Name: "web", Replicas: 2,
		Containers: []recommend.Container{{Name: "app", Requests: requests, Limits: limits}}}
}

var webKey = recommend.Key{Namespace: "shop", Kind: "Deployment", Name: "web", Container: "app"}

// sampled is a history of the workload with one usage sample at each of
// the given offsets from start.
func sampled(w recommend.Workload, usage recommend.Resources, offsets ...time.Duration) recommend.History {
	h := recommend.History{}
	for _, off := range offsets {
		h = h.Add(recommend.Snapshot{At: start.Add(off), Workloads: []recommend.Workload{w}, Metrics: true,
			Usage: map[recommend.Key]recommend.Resources{webKey: usage}}, nil)
	}
	return h
}

func TestRowsFollowTheRules(t *testing.T) {
	current := recommend.Resources{CPUMillis: 1000, MemoryBytes: 2 * gi}
	limits := recommend.Resources{CPUMillis: 2000, MemoryBytes: 4 * gi}
	used := recommend.Resources{CPUMillis: 203, MemoryBytes: 300 * mi}
	twoDays := []time.Duration{0, 12 * time.Hour, 25 * time.Hour, 48 * time.Hour}
	tests := []struct {
		name     string
		history  recommend.History
		at       time.Time
		source   recommend.Source
		requests recommend.Resources
		limits   recommend.Resources
		cpuPct   int32
		memPct   int32
		hours    int32
		changes  bool
	}{
		{name: "an estimate: the peak with 15%, rounded up, memory limit 20% above",
			history: sampled(web(current, limits), used, twoDays...), at: start.Add(48 * time.Hour),
			source: recommend.SourceUsage, requests: recommend.Resources{CPUMillis: 240, MemoryBytes: 345 * mi},
			limits: recommend.Resources{MemoryBytes: 414 * mi}, cpuPct: -76, memPct: -83, hours: 48, changes: true},
		{name: "less than a day of usage is not enough",
			history: sampled(web(current, limits), used, 0, 23*time.Hour), at: start.Add(23 * time.Hour),
			source: recommend.SourceInsufficient, hours: 23},
		{name: "never sampled", history: recommend.History{}.Add(recommend.Snapshot{At: start, Workloads: []recommend.Workload{web(current, limits)}}, nil),
			at: start, source: recommend.SourceNone},
		{name: "tiny usage stops at the floor",
			history: sampled(web(recommend.Resources{}, recommend.Resources{}), recommend.Resources{CPUMillis: 1, MemoryBytes: mi}, twoDays...),
			at:      start.Add(48 * time.Hour), source: recommend.SourceUsage,
			requests: recommend.Resources{CPUMillis: 10, MemoryBytes: 16 * mi}, limits: recommend.Resources{MemoryBytes: 20 * mi},
			hours: 48, changes: true},
		{name: "within a tenth of what is set is no change",
			history: sampled(web(recommend.Resources{CPUMillis: 250, MemoryBytes: 350 * mi}, limits), used, twoDays...),
			at:      start.Add(48 * time.Hour), source: recommend.SourceUsage,
			requests: recommend.Resources{CPUMillis: 240, MemoryBytes: 345 * mi}, limits: recommend.Resources{MemoryBytes: 414 * mi},
			cpuPct: -4, memPct: -1, hours: 48},
		{name: "usage older than the window no longer counts",
			history: sampled(web(current, limits), used, twoDays...), at: start.Add(48*time.Hour + recommend.Window + 24*time.Hour),
			source: recommend.SourceNone, hours: 48},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := recommend.Rows(tt.history, tt.at)

			if len(rows) != 1 {
				t.Fatalf("%d rows, want 1", len(rows))
			}
			r := rows[0]
			if r.Source != tt.source || r.RecommendedRequests != tt.requests || r.RecommendedLimits != tt.limits {
				t.Errorf("source %s requests %+v limits %+v, want %s %+v %+v", r.Source, r.RecommendedRequests, r.RecommendedLimits,
					tt.source, tt.requests, tt.limits)
			}
			if r.CPUPercent != tt.cpuPct || r.MemoryPercent != tt.memPct || r.DataHours != tt.hours || r.Changes() != tt.changes {
				t.Errorf("cpu %d%% memory %d%% data %dh changes %v, want %d%% %d%% %dh %v", r.CPUPercent, r.MemoryPercent,
					r.DataHours, r.Changes(), tt.cpuPct, tt.memPct, tt.hours, tt.changes)
			}
			if r.RecommendedLimits.CPUMillis != 0 {
				t.Errorf("a CPU limit of %dm is recommended; none ever is", r.RecommendedLimits.CPUMillis)
			}
		})
	}
}

// The difference is recommended minus current, and the peak and average
// are of the samples in the window.
func TestRowReportsUsageAndDifference(t *testing.T) {
	w := web(recommend.Resources{CPUMillis: 100, MemoryBytes: 128 * mi}, recommend.Resources{})
	h := sampled(w, recommend.Resources{CPUMillis: 100, MemoryBytes: 100 * mi}, 0, 24*time.Hour)
	h = h.Add(recommend.Snapshot{At: start.Add(30 * time.Hour), Workloads: []recommend.Workload{w}, Metrics: true,
		Usage: map[recommend.Key]recommend.Resources{webKey: {CPUMillis: 400, MemoryBytes: 400 * mi}}}, nil)

	r := recommend.Rows(h, start.Add(30*time.Hour))[0]

	if r.Peak != (recommend.Resources{CPUMillis: 400, MemoryBytes: 400 * mi}) || r.Average != (recommend.Resources{CPUMillis: 200, MemoryBytes: 200 * mi}) {
		t.Errorf("peak %+v average %+v", r.Peak, r.Average)
	}
	want := recommend.Resources{CPUMillis: 460 - 100, MemoryBytes: 460*mi - 128*mi}
	if r.Difference != want || r.CPUPercent != 360 || r.Samples != 3 {
		t.Errorf("difference %+v (%d%%), %d samples; want %+v (360%%), 3", r.Difference, r.CPUPercent, r.Samples, want)
	}
}

// A VPA's recommendation wins over the estimate, and needs no usage.
func TestVPARecommendationWins(t *testing.T) {
	w := web(recommend.Resources{CPUMillis: 500, MemoryBytes: gi}, recommend.Resources{})
	vpa := recommend.VPARecommendation{Target: recommend.Resources{CPUMillis: 120, MemoryBytes: 200 * mi},
		UpperBound: recommend.Resources{CPUMillis: 900, MemoryBytes: 512*mi + 1}}
	h := recommend.History{}.Add(recommend.Snapshot{At: start, Workloads: []recommend.Workload{w}, VPA: true,
		VPARecommendations: map[recommend.Key]recommend.VPARecommendation{webKey: vpa}}, nil)

	r := recommend.Rows(h, start)[0]

	if r.Source != recommend.SourceVPA || r.RecommendedRequests != vpa.Target ||
		r.RecommendedLimits != (recommend.Resources{MemoryBytes: 513 * mi}) {
		t.Fatalf("row = %+v", r)
	}
}

func TestHistoryAdd(t *testing.T) {
	w := web(recommend.Resources{CPUMillis: 100}, recommend.Resources{})
	used := recommend.Resources{CPUMillis: 50, MemoryBytes: 64 * mi}

	t.Run("does not change the history it is called on", func(t *testing.T) {
		h := sampled(w, used, 0)
		before := fmt.Sprintf("%+v", h)

		_ = h.Add(recommend.Snapshot{At: start.Add(time.Hour), Workloads: []recommend.Workload{w},
			Usage: map[recommend.Key]recommend.Resources{webKey: used}}, nil)

		if after := fmt.Sprintf("%+v", h); after != before {
			t.Errorf("history changed:\n%s\n%s", before, after)
		}
	})
	t.Run("one bucket a day, and days out of the window go", func(t *testing.T) {
		var offsets []time.Duration
		for hour := 0; hour <= 10*24; hour += 6 {
			offsets = append(offsets, time.Duration(hour)*time.Hour)
		}

		h := sampled(w, used, offsets...)

		c := h.Workloads[0].Containers[0]
		if len(c.Days) != 7 || c.Samples != len(offsets) {
			t.Fatalf("%d day buckets and %d samples, want 7 and %d", len(c.Days), c.Samples, len(offsets))
		}
		if c.Days[0].N != 4 || c.Days[0].CPUMax != 50 || c.Days[0].CPUSum != 200 {
			t.Errorf("a full day's bucket = %+v", c.Days[0])
		}
	})
	t.Run("a workload that is gone takes its record with it", func(t *testing.T) {
		h := sampled(w, used, 0)

		h = h.Add(recommend.Snapshot{At: start.Add(time.Hour)}, nil)

		if len(h.Workloads) != 0 {
			t.Errorf("workloads = %+v", h.Workloads)
		}
	})
	t.Run("the current requests replace the recorded ones", func(t *testing.T) {
		h := sampled(w, used, 0)
		resized := web(recommend.Resources{CPUMillis: 300}, recommend.Resources{})

		h = h.Add(recommend.Snapshot{At: start.Add(time.Hour), Workloads: []recommend.Workload{resized}}, nil)

		c := h.Workloads[0].Containers[0]
		if c.Requests.CPUMillis != 300 || c.Samples != 1 {
			t.Errorf("container = %+v, want the new request and the old sample", c)
		}
	})
	t.Run("platform namespaces are marked", func(t *testing.T) {
		h := recommend.History{}.Add(recommend.Snapshot{At: start, Workloads: []recommend.Workload{w}},
			func(ns string) bool { return ns == "shop" })

		if !h.Workloads[0].Platform || !recommend.Rows(h, start)[0].Platform {
			t.Error("the workload is not marked as the platform's")
		}
	})
}

// many is a snapshot of n single-container workloads with full names and
// usage that grows with the index.
func many(n int, at time.Time) recommend.Snapshot {
	s := recommend.Snapshot{At: at, Metrics: true, Usage: map[recommend.Key]recommend.Resources{}}
	for i := range n {
		w := recommend.Workload{Namespace: fmt.Sprintf("team-%03d-production", i%40), Kind: "Deployment",
			Name: fmt.Sprintf("service-%05d-backend-api", i), Replicas: 3,
			Containers: []recommend.Container{{Name: "application", Requests: recommend.Resources{CPUMillis: 500, MemoryBytes: gi},
				Limits: recommend.Resources{CPUMillis: 1000, MemoryBytes: 2 * gi}}}}
		s.Workloads = append(s.Workloads, w)
		s.Usage[recommend.Key{Namespace: w.Namespace, Kind: w.Kind, Name: w.Name, Container: "application"}] =
			recommend.Resources{CPUMillis: int64(100 + i + at.Hour()), MemoryBytes: int64(i+1)*mi + int64(at.Hour())}
	}
	return s
}

// The history is bounded: past MaxContainers the largest consumers stay,
// and a full week of every one of them fits a ConfigMap.
func TestHistoryIsBounded(t *testing.T) {
	h := recommend.History{}
	for hour := 0; hour < 8*24; hour += 6 {
		h = h.Add(many(recommend.MaxContainers+500, start.Add(time.Duration(hour)*time.Hour)), nil)
	}

	raw, encoded, err := h.Encode()

	if err != nil {
		t.Fatal(err)
	}
	if n := h.Containers(); n != recommend.MaxContainers || !h.Truncated || h.Dropped != 500 {
		t.Errorf("%d containers, truncated %v, dropped %d; want %d, true, 500", n, h.Truncated, h.Dropped, recommend.MaxContainers)
	}
	if len(raw) > recommend.MaxEncoded || encoded.Containers() != recommend.MaxContainers {
		t.Errorf("%d bytes for %d containers, want at most %d for all %d", len(raw), encoded.Containers(), recommend.MaxEncoded, recommend.MaxContainers)
	}
	t.Logf("%d containers with 7 day buckets: %d bytes gzipped, %d a container", encoded.Containers(), len(raw), len(raw)/encoded.Containers())
	// The smallest consumers are the ones dropped.
	for _, w := range h.Workloads {
		if strings.HasPrefix(w.Name, "service-00000") {
			t.Fatalf("%s was kept: the smallest consumers go first", w.Name)
		}
	}
	back, err := recommend.Decode(raw)
	if err != nil || back.Containers() != encoded.Containers() || !back.SampledAt.Equal(encoded.SampledAt) {
		t.Errorf("decoded %d containers at %s, %v", back.Containers(), back.SampledAt, err)
	}
}

func TestDecode(t *testing.T) {
	if h, err := recommend.Decode(nil); err != nil || h.Containers() != 0 {
		t.Errorf("no history yet: %+v, %v", h, err)
	}
	if _, err := recommend.Decode([]byte("not gzip")); err == nil {
		t.Error("garbage was decoded")
	}
}

// fakeReader is a cluster as a Reader sees it.
type fakeReader struct {
	workloads  []recommend.Workload
	pods       []recommend.Pod
	usage      []recommend.PodUsage
	metrics    bool
	metricsErr error
	vpas       []recommend.VPA
	vpa        bool
	workErr    error
}

func (f fakeReader) Workloads(context.Context) ([]recommend.Workload, error) {
	return f.workloads, f.workErr
}
func (f fakeReader) Pods(context.Context) ([]recommend.Pod, error) { return f.pods, nil }
func (f fakeReader) PodMetrics(context.Context) ([]recommend.PodUsage, bool, error) {
	return f.usage, f.metrics, f.metricsErr
}
func (f fakeReader) VPAs(context.Context) ([]recommend.VPA, bool, error) { return f.vpas, f.vpa, nil }

func TestSample(t *testing.T) {
	db := recommend.Workload{Namespace: "shop", Kind: "StatefulSet", Name: "db", Replicas: 1,
		Containers: []recommend.Container{{Name: "postgres"}}}
	cluster := fakeReader{
		workloads: []recommend.Workload{web(recommend.Resources{}, recommend.Resources{}), db},
		pods: []recommend.Pod{
			{Namespace: "shop", Name: "web-7d9f-abc", OwnerKind: "ReplicaSet", OwnerName: "web-7d9f"},
			{Namespace: "shop", Name: "web-7d9f-def", OwnerKind: "ReplicaSet", OwnerName: "web-7d9f"},
			{Namespace: "shop", Name: "db-0", OwnerKind: "StatefulSet", OwnerName: "db"},
			{Namespace: "shop", Name: "job-x", OwnerKind: "Job", OwnerName: "job"},
			{Namespace: "shop", Name: "other-1-abc", OwnerKind: "ReplicaSet", OwnerName: "other-1"},
		},
		usage: []recommend.PodUsage{
			{Namespace: "shop", Pod: "web-7d9f-abc", Containers: map[string]recommend.Resources{"app": {CPUMillis: 100, MemoryBytes: 200 * mi}}},
			{Namespace: "shop", Pod: "web-7d9f-def", Containers: map[string]recommend.Resources{"app": {CPUMillis: 300, MemoryBytes: 150 * mi}}},
			{Namespace: "shop", Pod: "db-0", Containers: map[string]recommend.Resources{"postgres": {CPUMillis: 50, MemoryBytes: gi}}},
			{Namespace: "shop", Pod: "job-x", Containers: map[string]recommend.Resources{"run": {CPUMillis: 999}}},
			{Namespace: "shop", Pod: "other-1-abc", Containers: map[string]recommend.Resources{"app": {CPUMillis: 999}}},
		},
		metrics: true, vpa: true,
		vpas: []recommend.VPA{
			{Namespace: "shop", TargetKind: "StatefulSet", TargetName: "db",
				Containers: map[string]recommend.VPARecommendation{"postgres": {Target: recommend.Resources{CPUMillis: 80}}}},
			{Namespace: "shop", TargetKind: "Deployment", TargetName: "gone",
				Containers: map[string]recommend.VPARecommendation{"app": {Target: recommend.Resources{CPUMillis: 1}}}},
		},
	}
	dbKey := recommend.Key{Namespace: "shop", Kind: "StatefulSet", Name: "db", Container: "postgres"}

	t.Run("usage is the most any pod of the workload uses", func(t *testing.T) {
		s, err := recommend.Sample(t.Context(), cluster, start)

		if err != nil || !s.Metrics || !s.VPA || len(s.Notes) != 0 {
			t.Fatalf("sample = %+v, %v", s, err)
		}
		if got := s.Usage[webKey]; got != (recommend.Resources{CPUMillis: 300, MemoryBytes: 200 * mi}) {
			t.Errorf("web usage = %+v", got)
		}
		if len(s.Usage) != 2 || s.Usage[dbKey].MemoryBytes != gi {
			t.Errorf("usage = %+v, want the two workloads only", s.Usage)
		}
		if len(s.VPARecommendations) != 1 || s.VPARecommendations[dbKey].Target.CPUMillis != 80 {
			t.Errorf("vpa = %+v, want the one of a workload that exists", s.VPARecommendations)
		}
	})
	t.Run("no metrics API and no VPA", func(t *testing.T) {
		bare := cluster
		bare.metrics, bare.vpa, bare.usage, bare.vpas = false, false, nil, nil

		s, err := recommend.Sample(t.Context(), bare, start)

		if err != nil || s.Metrics || s.VPA || len(s.Usage) != 0 || len(s.Workloads) != 2 {
			t.Fatalf("sample = %+v, %v", s, err)
		}
	})
	t.Run("a metrics API that is down is a note", func(t *testing.T) {
		down := cluster
		down.metricsErr = errors.New("service unavailable")

		s, err := recommend.Sample(t.Context(), down, start)

		if err != nil || len(s.Notes) != 1 || !strings.Contains(s.Notes[0], "service unavailable") || len(s.Usage) != 0 {
			t.Fatalf("sample = %+v, %v", s, err)
		}
	})
	t.Run("workloads that cannot be listed fail the sample", func(t *testing.T) {
		broken := cluster
		broken.workErr = errors.New("forbidden")

		if _, err := recommend.Sample(t.Context(), broken, start); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("Sample = %v", err)
		}
	})
}
