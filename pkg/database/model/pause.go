package model

import (
	"encoding/json"
	"slices"
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
	// Scopes are what the pause was asked for at: the tenant, the network
	// or the cluster itself. A pause of the tenant and one of the row's
	// own network end separately (With, Without). Empty: a pause of an
	// older build, which any resume ends.
	Scopes []PauseScope `json:"scopes,omitempty"`
}

// PauseScope is what a pause was asked for: a tenant (everything it has)
// or one network or cluster.
type PauseScope string

// The scopes of a pause.
const (
	PauseTenant  PauseScope = "tenant"
	PauseNetwork PauseScope = "network"
	PauseCluster PauseScope = "cluster"
)

// With is p paused also at scope: a row that is not paused gets pause
// (at scope); one that is keeps who paused it first, since when and why,
// and adds scope. A pause without scopes (an older build's) is returned
// as it is. p is not changed.
func (p *Pause) With(scope PauseScope, pause *Pause) *Pause {
	if p == nil {
		out := *pause
		out.Scopes = []PauseScope{scope}
		return &out
	}
	if len(p.Scopes) == 0 || slices.Contains(p.Scopes, scope) {
		return p
	}
	out := *p
	out.Scopes = append(slices.Clone(p.Scopes), scope)
	return &out
}

// Without is p after a resume at scope: nil when the pause ends. own is a
// resume of the row's own scope (its network, its cluster): it ends the
// pause whatever else holds it, so one network or cluster of a paused
// tenant can run again. A resume of a wider scope (the tenant) removes
// that scope only: a row also paused on its own stays paused. A pause
// without scopes ends at any resume. p is not changed.
func (p *Pause) Without(scope PauseScope, own bool) *Pause {
	if p == nil || own || len(p.Scopes) == 0 {
		return nil
	}
	rest := slices.DeleteFunc(slices.Clone(p.Scopes), func(s PauseScope) bool { return s == scope })
	if len(rest) == 0 {
		return nil
	}
	out := *p
	out.Scopes = rest
	return &out
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

// PauseTarget is what a pause is set on: one cluster (ClusterID), one
// network (NetworkID), or with neither the whole tenant. At most one is
// set.
type PauseTarget struct {
	NetworkID, ClusterID string
}

// Scope is the scope a pause of the target is recorded at.
func (t PauseTarget) Scope() PauseScope {
	switch {
	case t.ClusterID != "":
		return PauseCluster
	case t.NetworkID != "":
		return PauseNetwork
	default:
		return PauseTenant
	}
}
