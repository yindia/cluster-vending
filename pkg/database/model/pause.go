package model

import (
	"encoding/json"
	"time"
)

// Pause is an operator's pause of a tenant's account, network or cluster
// (AdminService.PauseReconcile): the controller does not act on a paused
// row, it only observes it. The store keeps it as an annotation of the
// row's object, set and removed by Tenants.SetPause only: a row update
// neither sets nor removes it.
type Pause struct {
	// By is who paused: the caller's name, "anonymous" without
	// authentication.
	By string `json:"by"`
	// Since is when.
	Since time.Time `json:"since"`
	// Reason is why, as the caller said. Optional.
	Reason string `json:"reason,omitempty"`
}

// unknownPauser is who paused when the annotation does not say: it was
// written by hand.
const unknownPauser = "unknown"

// Annotation is the pause as the annotation's value: JSON.
func (p Pause) Annotation() string {
	p.Since = p.Since.UTC().Truncate(time.Second)
	raw, _ := json.Marshal(p) // strings and a time: cannot fail
	return string(raw)
}

// ParsePause reads an annotation's value: nil for none. A value that is
// not what Annotation writes (set by hand, e.g. "true") still pauses: by
// "unknown", since no known time.
func ParsePause(value string) *Pause {
	if value == "" {
		return nil
	}
	p := &Pause{}
	if err := json.Unmarshal([]byte(value), p); err != nil || p.By == "" {
		return &Pause{By: unknownPauser, Since: p.Since}
	}
	return p
}
