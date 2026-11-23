package live

import "time"

// Limits are the caps, budgets and windows of PROP-02 5.9. One replica runs
// in version 1 (5.5): every limit is that process's. The values are the
// proposal's starting values; the phase 3 load test is to measure them.
type Limits struct {
	// MaxNodeRows: a nodes build over it stops (LIMIT_EXCEEDED).
	MaxNodeRows int
	// PageLimit is the list page size (limit); MaxPages the pages of a build.
	PageLimit int64
	MaxPages  int
	// NodesDeadline bounds a nodes build.
	NodesDeadline time.Duration
	// Reuse: a build this young serves a new request; ViewIdle and ViewMaxAge
	// bound how long a view (and its tokens) lives after its last use and
	// after its build.
	Reuse, ViewIdle, ViewMaxAge time.Duration
	// BuildsPerClusterMinute and BuildsPerTenantMinute: builds started in the
	// last minute, per cluster and per tenant.
	BuildsPerClusterMinute, BuildsPerTenantMinute int
	// FleetInFlight builds at once; a build waits FleetWait for a slot, then
	// is refused as BUSY.
	FleetInFlight int
	FleetWait     time.Duration
	// Cooldown after a failure; LongCooldown after FailuresForLong failures
	// in a row. No request reaches the cluster meanwhile.
	Cooldown, LongCooldown time.Duration
	FailuresForLong        int
	// ViewBytes bounds the views kept, in total; the least recently used go
	// first.
	ViewBytes int64
	// DefaultPageSize and MaxPageSize are the rows of a response.
	DefaultPageSize, MaxPageSize int
	// QPS and Burst are the client-side limiter of one cluster's client.
	QPS   float32
	Burst int
}

// DefaultLimits are the values of PROP-02 5.9.
func DefaultLimits() Limits {
	return Limits{
		MaxNodeRows: 5000, PageLimit: 500, MaxPages: 200, NodesDeadline: 30 * time.Second,
		Reuse: 15 * time.Second, ViewIdle: time.Minute, ViewMaxAge: 5 * time.Minute,
		BuildsPerClusterMinute: 4, BuildsPerTenantMinute: 10, FleetInFlight: 8, FleetWait: 2 * time.Second,
		Cooldown: 30 * time.Second, LongCooldown: 2 * time.Minute, FailuresForLong: 3,
		ViewBytes:       256 << 20,
		DefaultPageSize: 100, MaxPageSize: 500, QPS: 10, Burst: 20,
	}
}
