package helmrel

import (
	"math"
	"time"
)

// The design's defaults (--helm-backoff-max, --helm-max-failures).
const (
	DefaultBackoffMax  = 30 * time.Minute
	DefaultMaxFailures = 5
	firstDelay         = time.Minute
	// jitterShare is how much of a delay may be added at random, so the
	// releases of a hundred clusters do not retry at the same second.
	jitterShare = 0.25
)

// Delay is how long to wait after attempt (1 is the first failure): one
// minute, doubling, capped at max. jitter in [0, 1] adds up to a quarter of
// the delay; the cap still holds.
func Delay(attempt int, max time.Duration, jitter float64) time.Duration {
	if max <= 0 {
		max = DefaultBackoffMax
	}
	if attempt < 1 {
		attempt = 1
	}
	d := max
	if attempt <= 30 { // beyond that the delay is far past any cap
		d = time.Duration(float64(firstDelay) * math.Pow(2, float64(attempt-1)))
	}
	d += time.Duration(float64(d) * jitterShare * min(max1(jitter), 1))
	return min(d, max)
}

func max1(f float64) float64 {
	if f < 0 {
		return 0
	}
	return f
}
