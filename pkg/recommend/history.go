package recommend

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
)

// The bounds of a history. A container's record is its name, its current
// requests and limits, an optional VPA recommendation, two timestamps, a
// sample count and at most windowDays day buckets of six integers: about
// 600 bytes of JSON at its fullest. Gzipped, MaxContainers such records
// of regular usage take about 100 KiB (the tests measure it), and less
// regular ones a few times that: under MaxEncoded, the size a ConfigMap
// carries with room to spare (1 MiB is the object's limit). Encode holds
// that bound whatever the data: it drops the smallest consumers until
// the history fits.
const (
	// MaxContainers is how many containers a history keeps. Past it the
	// largest consumers are kept and the rest dropped (Truncated).
	MaxContainers = 2000
	// MaxEncoded is the most bytes Encode returns.
	MaxEncoded = 900 << 10

	windowDays     = int64(Window / (24 * time.Hour))
	secondsPerDay  = 24 * 60 * 60
	historyVersion = 1
	maxDecoded     = 32 << 20
)

// History is the rolling usage record of one cluster: every workload as it
// was last seen, and per container the usage of the last Window in day
// buckets. It is plain data, stored gzipped (Encode, Decode).
type History struct {
	Version   int       `json:"v"`
	SampledAt time.Time `json:"sampledAt"`
	// Metrics and VPA: the last sample found the metrics API and the VPA
	// kind in the cluster.
	Metrics bool `json:"metrics,omitempty"`
	VPA     bool `json:"vpa,omitempty"`
	// Truncated: the cluster has more containers than are kept; Dropped
	// is how many were left out.
	Truncated bool             `json:"truncated,omitempty"`
	Dropped   int              `json:"dropped,omitempty"`
	Workloads []WorkloadRecord `json:"workloads,omitempty"`
}

// WorkloadRecord is a workload as the last sample saw it.
type WorkloadRecord struct {
	Namespace  string            `json:"ns"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Replicas   int32             `json:"replicas,omitempty"`
	Platform   bool              `json:"platform,omitempty"`
	Containers []ContainerRecord `json:"containers"`
}

// ContainerRecord is one container's current resources and its usage.
type ContainerRecord struct {
	Name     string             `json:"name"`
	Requests Resources          `json:"req,omitempty"`
	Limits   Resources          `json:"lim,omitempty"`
	VPA      *VPARecommendation `json:"vpa,omitempty"`
	// First and Last are when usage was first and last recorded (Unix
	// seconds); Samples how often.
	First   int64 `json:"first,omitempty"`
	Last    int64 `json:"last,omitempty"`
	Samples int   `json:"n,omitempty"`
	// Days are the day buckets, oldest first, at most one per day of the
	// window.
	Days []Day `json:"days,omitempty"`
}

// Day is the usage of one UTC day: how many samples, their maximum and
// their sum (for the average).
type Day struct {
	Day    int64 `json:"d"`
	N      int64 `json:"n"`
	CPUMax int64 `json:"cmax"`
	CPUSum int64 `json:"csum"`
	MemMax int64 `json:"mmax"`
	MemSum int64 `json:"msum"`
}

// usage is the peak, the sum and the number of samples of the days still
// in the window at now.
func (c ContainerRecord) usage(now time.Time) (peak, sum Resources, n int64) {
	oldest := now.Unix()/secondsPerDay - windowDays + 1
	for _, d := range c.Days {
		if d.Day < oldest {
			continue
		}
		peak.CPUMillis, peak.MemoryBytes = max(peak.CPUMillis, d.CPUMax), max(peak.MemoryBytes, d.MemMax)
		sum.CPUMillis, sum.MemoryBytes = sum.CPUMillis+d.CPUSum, sum.MemoryBytes+d.MemSum
		n += d.N
	}
	return peak, sum, n
}

// Containers is how many containers the history holds.
func (h History) Containers() int {
	n := 0
	for _, w := range h.Workloads {
		n += len(w.Containers)
	}
	return n
}

// Add returns the history after a sample: the workloads are those of the
// snapshot (a workload that is gone takes its record with it), each
// container keeps the usage recorded for it so far, gains the snapshot's
// usage in today's bucket and loses the days that left the window. h is
// not changed. platform says which namespaces are the platform's.
func (h History) Add(s Snapshot, platform func(namespace string) bool) History {
	before := map[Key]ContainerRecord{}
	for _, w := range h.Workloads {
		for _, c := range w.Containers {
			before[Key{w.Namespace, w.Kind, w.Name, c.Name}] = c
		}
	}
	today := s.At.Unix() / secondsPerDay
	out := History{Version: historyVersion, SampledAt: s.At.UTC(), Metrics: s.Metrics, VPA: s.VPA,
		Workloads: make([]WorkloadRecord, 0, len(s.Workloads))}
	for _, w := range s.Workloads {
		rec := WorkloadRecord{Namespace: w.Namespace, Kind: w.Kind, Name: w.Name, Replicas: w.Replicas,
			Platform: platform != nil && platform(w.Namespace), Containers: make([]ContainerRecord, 0, len(w.Containers))}
		for _, c := range w.Containers {
			key := Key{w.Namespace, w.Kind, w.Name, c.Name}
			old := before[key]
			cr := ContainerRecord{Name: c.Name, Requests: c.Requests, Limits: c.Limits,
				First: old.First, Last: old.Last, Samples: old.Samples}
			for _, d := range old.Days {
				if d.Day > today-windowDays {
					cr.Days = append(cr.Days, d)
				}
			}
			if v, ok := s.VPARecommendations[key]; ok {
				cr.VPA = &v
			}
			if u, ok := s.Usage[key]; ok {
				cr.record(u, s.At.Unix(), today)
			}
			rec.Containers = append(rec.Containers, cr)
		}
		out.Workloads = append(out.Workloads, rec)
	}
	return out.capped(MaxContainers)
}

// record adds one sample to today's bucket.
func (c *ContainerRecord) record(u Resources, at, today int64) {
	if c.Samples == 0 || c.First == 0 {
		c.First = at
	}
	c.Last = at
	c.Samples++
	if n := len(c.Days); n == 0 || c.Days[n-1].Day != today {
		c.Days = append(c.Days, Day{Day: today})
	}
	d := &c.Days[len(c.Days)-1]
	d.N++
	d.CPUMax, d.MemMax = max(d.CPUMax, u.CPUMillis), max(d.MemMax, u.MemoryBytes)
	d.CPUSum, d.MemSum = d.CPUSum+u.CPUMillis, d.MemSum+u.MemoryBytes
}

// score ranks a workload by what it consumes or asks for: the larger of
// peak usage and requests, memory in bytes plus CPU at 4 MiB a millicore.
func (w WorkloadRecord) score() int64 {
	var total int64
	for _, c := range w.Containers {
		var peak Resources
		for _, d := range c.Days {
			peak.CPUMillis, peak.MemoryBytes = max(peak.CPUMillis, d.CPUMax), max(peak.MemoryBytes, d.MemMax)
		}
		total += max(peak.MemoryBytes, c.Requests.MemoryBytes) + max(peak.CPUMillis, c.Requests.CPUMillis)*platformMemoryBias
	}
	return total * int64(max(w.Replicas, 1))
}

// capped is h with at most limit containers: the workloads that consume
// the most are kept, in their order. h is not changed.
func (h History) capped(limit int) History {
	total := h.Containers()
	if total <= limit {
		return h
	}
	order := make([]int, len(h.Workloads))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(h.Workloads[b].score(), h.Workloads[a].score())
	})
	keep, kept := make([]bool, len(h.Workloads)), 0
	for _, i := range order {
		if n := len(h.Workloads[i].Containers); kept+n <= limit {
			keep[i], kept = true, kept+n
		}
	}
	out := h
	out.Workloads = make([]WorkloadRecord, 0, len(h.Workloads))
	for i, w := range h.Workloads {
		if keep[i] {
			out.Workloads = append(out.Workloads, w)
		}
	}
	out.Truncated, out.Dropped = true, h.Dropped+total-kept
	return out
}

// Encode is the history as gzipped JSON of at most MaxEncoded bytes: when
// it is larger, the smallest consumers are dropped until it fits. The
// history returned is the one that was encoded.
func (h History) Encode() ([]byte, History, error) {
	for {
		raw, err := h.encode()
		if err != nil {
			return nil, h, err
		}
		if len(raw) <= MaxEncoded {
			return raw, h, nil
		}
		n := h.Containers()
		if n <= 1 {
			return nil, h, errors.New("usage history: one container does not fit")
		}
		h = h.capped(n * 9 / 10)
	}
}

func (h History) encode() ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(zw).Encode(h); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decode reads what Encode wrote. Empty input is an empty history.
func Decode(raw []byte) (History, error) {
	if len(raw) == 0 {
		return History{Version: historyVersion}, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return History{}, fmt.Errorf("usage history: %w", err)
	}
	defer func() { _ = zr.Close() }()
	data, err := io.ReadAll(io.LimitReader(zr, maxDecoded+1))
	if err != nil {
		return History{}, fmt.Errorf("usage history: %w", err)
	}
	if len(data) > maxDecoded {
		return History{}, errors.New("usage history: larger than 32 MiB")
	}
	var h History
	if err := json.Unmarshal(data, &h); err != nil {
		return History{}, fmt.Errorf("usage history: %w", err)
	}
	if h.Version != historyVersion {
		return History{}, fmt.Errorf("usage history: version %d, this build reads %d", h.Version, historyVersion)
	}
	return h, nil
}
