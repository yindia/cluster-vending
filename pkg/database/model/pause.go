package model

import (
	"encoding/json"
	"slices"
	"strings"
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
	// Until, when set, ends the pause by itself (Active). On a pause
	// asked for, it is that pause's; on a stored one, the first scope's.
	Until *time.Time `json:"until,omitempty"`
	// Entries are the scopes with who paused at each, when, why and until
	// when, in the order they were paused at. By, Since, Reason and Until
	// above are the first entry's. Empty on a pause of an older build: its
	// scopes then read as entries of its own fields (ParsePause).
	Entries []PauseEntry `json:"entries,omitempty"`
}

// PauseEntry is one scope of a pause, as it was asked for.
type PauseEntry struct {
	Scope  PauseScope `json:"scope"`
	By     string     `json:"by"`
	Since  time.Time  `json:"since"`
	Reason string     `json:"reason,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
}

// ended reports whether the entry's until has passed at now.
func (e PauseEntry) ended(now time.Time) bool { return e.Until != nil && !e.Until.After(now) }

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
	entry := PauseEntry{Scope: scope, By: pause.By, Since: pause.Since, Reason: pause.Reason, Until: pause.Until}
	if p == nil {
		out := *pause
		out.Scopes, out.Entries = []PauseScope{scope}, []PauseEntry{entry}
		return &out
	}
	if len(p.Scopes) == 0 || slices.Contains(p.Scopes, scope) {
		return p
	}
	out := *p
	out.Scopes = append(slices.Clone(p.Scopes), scope)
	out.Entries = append(p.entries(), entry)
	return &out
}

// entries are p's entries: as stored, or, for a pause of an older build,
// one per scope with the pause's own who, when and why.
func (p *Pause) entries() []PauseEntry {
	if len(p.Entries) > 0 {
		return slices.Clone(p.Entries)
	}
	out := make([]PauseEntry, 0, len(p.Scopes))
	for _, s := range p.Scopes {
		out = append(out, PauseEntry{Scope: s, By: p.By, Since: p.Since, Reason: p.Reason, Until: p.Until})
	}
	return out
}

// Entry is the entry of scope; false when p is not paused at it.
func (p *Pause) Entry(scope PauseScope) (PauseEntry, bool) {
	if p == nil {
		return PauseEntry{}, false
	}
	for _, e := range p.entries() {
		if e.Scope == scope {
			return e, true
		}
	}
	return PauseEntry{}, false
}

// Active is p at now: without the scopes whose until has passed; nil when
// none is left. A pause without scopes (an older build's) ends at its own
// until, if it has one. p is not changed.
func (p *Pause) Active(now time.Time) *Pause {
	if p == nil {
		return nil
	}
	if len(p.Scopes) == 0 {
		if p.Until != nil && !p.Until.After(now) {
			return nil
		}
		return p
	}
	all := p.entries()
	kept := slices.DeleteFunc(slices.Clone(all), func(e PauseEntry) bool { return e.ended(now) })
	switch {
	case len(kept) == 0:
		return nil
	case len(kept) == len(all):
		return p
	}
	return fromEntries(kept)
}

// NextEnd is the earliest until among p's scopes: when part of the pause
// ends by itself; false when none has an until.
func (p *Pause) NextEnd() (time.Time, bool) {
	if p == nil {
		return time.Time{}, false
	}
	var out time.Time
	for _, e := range p.entries() {
		if e.Until != nil && (out.IsZero() || e.Until.Before(out)) {
			out = *e.Until
		}
	}
	if out.IsZero() && p.Until != nil {
		out = *p.Until
	}
	return out, !out.IsZero()
}

// Lapsed reports whether some scope of p ended by its until at now: the
// stored pause says more than what holds, and is to be written again
// without it.
func (p *Pause) Lapsed(now time.Time) bool {
	return p != nil && p.Active(now) != p
}

// fromEntries is the pause of entries (at least one): the first one's
// who, when, why and until.
func fromEntries(entries []PauseEntry) *Pause {
	first := entries[0]
	out := &Pause{By: first.By, Since: first.Since, Reason: first.Reason, Until: first.Until, Entries: entries}
	for _, e := range entries {
		out.Scopes = append(out.Scopes, e.Scope)
	}
	return out
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
	rest := slices.DeleteFunc(p.entries(), func(e PauseEntry) bool { return e.Scope == scope })
	if len(rest) == 0 {
		return nil
	}
	return fromEntries(rest)
}

// unknownPauser is who paused when the annotation does not say: it was
// written by hand.
const unknownPauser = "unknown"

// Annotation is the pause as the annotation's value: JSON.
func (p Pause) Annotation() string {
	p.Since = p.Since.UTC().Truncate(time.Second)
	p.Entries = slices.Clone(p.Entries)
	for i := range p.Entries {
		p.Entries[i].Since = p.Entries[i].Since.UTC().Truncate(time.Second)
	}
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
	if len(p.Entries) == 0 && len(p.Scopes) > 0 {
		p.Entries = p.entries()
	}
	return p
}

// channelScopePrefix starts the scope of a pause asked for every cluster
// of a release channel, or of one tier of it: "channel:beta",
// "channel:beta/tier:0".
const (
	channelScopePrefix = "channel:"
	tierScopeSeparator = "/tier:"
)

// ChannelScope is the scope of a pause of a channel's clusters (tier "":
// every tier), as each of them records it. The channel is lowercase.
func ChannelScope(channel, tier string) PauseScope {
	s := channelScopePrefix + strings.ToLower(channel)
	if tier != "" {
		s += tierScopeSeparator + tier
	}
	return PauseScope(s)
}

// Channel is the channel and tier of a channel's scope; false for any
// other scope.
func (s PauseScope) Channel() (channel, tier string, ok bool) {
	rest, ok := strings.CutPrefix(string(s), channelScopePrefix)
	if !ok {
		return "", "", false
	}
	channel, tier, _ = strings.Cut(rest, tierScopeSeparator)
	return channel, tier, true
}

// Text is the scope for people: "tenant", "network", "cluster", "channel
// beta", "channel beta tier 0".
func (s PauseScope) Text() string {
	channel, tier, ok := s.Channel()
	switch {
	case !ok:
		return string(s)
	case tier != "":
		return "channel " + channel + " tier " + tier
	}
	return "channel " + channel
}

// PauseTarget is what a pause is set on: one cluster (ClusterID), one
// network (NetworkID), or with neither the whole tenant. At most one is
// set. Selector, with ClusterID, records the pause of that cluster at a
// wider scope it was asked for (ChannelScope): a resume of that scope
// removes it, and leaves the cluster's other pauses.
type PauseTarget struct {
	NetworkID, ClusterID string
	Selector             PauseScope
}

// Scope is the scope a pause of the target is recorded at.
func (t PauseTarget) Scope() PauseScope {
	switch {
	case t.Selector != "":
		return t.Selector
	case t.ClusterID != "":
		return PauseCluster
	case t.NetworkID != "":
		return PauseNetwork
	default:
		return PauseTenant
	}
}
