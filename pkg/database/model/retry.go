package model

import (
	"encoding/json"
	"slices"
	"time"
)

// Retry is a request to recover what failed on a cluster
// (ClusterService.RetryCluster): the API server records it on the Cluster
// object and the controller acts on it once, then removes it. Components
// are the failed platform or tenant components to try again; Upgrade
// asks to try a failed automatic upgrade again.
type Retry struct {
	At         time.Time `json:"at"`
	By         string    `json:"by,omitempty"`
	Components []string  `json:"components,omitempty"`
	Upgrade    bool      `json:"upgrade,omitempty"`
}

// ParseRetry reads the annotation; nil for none or one this build cannot
// read (it is then left for the request to be made again).
func ParseRetry(value string) *Retry {
	if value == "" {
		return nil
	}
	r := &Retry{}
	if err := json.Unmarshal([]byte(value), r); err != nil || r.At.IsZero() {
		return nil
	}
	return r
}

// Annotation is the retry as the annotation holds it.
func (r *Retry) Annotation() string {
	b, _ := json.Marshal(r) //nolint:errchkjson // a struct of strings, a time and a bool
	return string(b)
}

// Has reports whether the retry asks for the component.
func (r *Retry) Has(component string) bool {
	return r != nil && slices.Contains(r.Components, component)
}

// Merge is r with next added: what was asked before and not yet acted on
// is kept; the time and who asked are next's.
func (r *Retry) Merge(next Retry) *Retry {
	out := next
	out.Components = slices.Clone(next.Components)
	if r != nil {
		for _, c := range r.Components {
			if !slices.Contains(out.Components, c) {
				out.Components = append(out.Components, c)
			}
		}
		out.Upgrade = out.Upgrade || r.Upgrade
	}
	slices.Sort(out.Components)
	return &out
}
