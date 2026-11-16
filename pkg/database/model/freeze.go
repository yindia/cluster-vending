package model

import "time"

// Freeze is the platform freeze (AdminService.Freeze) as one cluster or
// network sees it. Held (LetThrough false), the controller does not act on
// the resource and its managed resources carry crossplane.io/paused; let
// through, it runs as if the platform were not frozen.
type Freeze struct {
	Scope string
	// By is who froze: the caller's name, "anonymous" without
	// authentication.
	By string
	// Since is when.
	Since time.Time
	// Reason is why, as the caller said.
	Reason string
	// Until, when set, ends the freeze by itself.
	Until *time.Time
	// LetThrough: the freeze lets this resource through (admin unfreeze
	// --cluster or --network).
	LetThrough bool
}

// Holds reports whether f keeps the platform from acting on the resource:
// the platform is frozen and the resource is not let through.
func (f *Freeze) Holds() bool { return f != nil && !f.LetThrough }
