package helmrel

import "strings"

// ErrorClass says what kind of failure a Release reports.
type ErrorClass string

// The classes of a Release's last error.
const (
	// ClassNone: no error.
	ClassNone ErrorClass = ""
	// ClassTransient: the workload cluster or Helm's lock was briefly in
	// the way (API unreachable, timeouts, throttling, a lock to clear).
	// Retried quietly with backoff; never a failure.
	ClassTransient ErrorClass = "transient"
	// ClassChart: the chart could not be fetched (repository down, chart or
	// version not found, pull failed). Retried with backoff and reported;
	// it never makes the platform give up, the chart may come back.
	ClassChart ErrorClass = "chart"
	// ClassRelease: Helm ran and the release failed (a hook failed, the
	// rollout timed out, values are invalid). Counted.
	ClassRelease ErrorClass = "release"
)

// Counts reports whether an error of the class counts toward giving up.
func (c ErrorClass) Counts() bool { return c == ClassRelease }

// String is the class as the status reports it.
func (c ErrorClass) String() string { return string(c) }

// The phrases below are matched case-insensitively against provider-helm's
// message, which wraps Helm's and the Kubernetes client's.
var (
	// transient: the cluster could not be reached, or answered "later".
	transientPhrases = []string{
		"could not get server version",
		"tls handshake timeout",
		"i/o timeout",
		"connection refused",
		"connection reset",
		"no such host",
		"client.timeout exceeded",
		"unexpected eof",
		"server is currently unable to handle the request",
		"too many requests",
		"rate limit",
		"throttl",
		"service unavailable",
		"etcdserver: request timed out",
		"unable to connect to the server",
		"net/http: request canceled",
		// Helm's lock, left behind by an interrupted operation: the
		// recovery (platform_recover.go) clears it once it is old enough.
		"another operation (install/upgrade/rollback) is in progress",
		"has no deployed releases", // an uninstall that never finished
	}
	// chart: the chart did not come.
	chartPhrases = []string{
		"failed to locate chart",
		"failed to pull",
		"failed to download",
		"failed to fetch",
		"no chart version found",
		"not a valid chart repository",
		"manifest unknown",
		"chart not found",
		"not found in",
		"failed to load chart",
		"cannot load repository index",
	}
)

// Classify says which class of failure message is. An empty message is
// ClassNone; one nobody has seen is ClassRelease, so an unknown failure is
// counted rather than retried for ever.
func Classify(message string) ErrorClass {
	m := strings.ToLower(strings.TrimSpace(message))
	switch {
	case m == "":
		return ClassNone
	case containsAny(m, transientPhrases):
		return ClassTransient
	case containsAny(m, chartPhrases):
		return ClassChart
	default:
		return ClassRelease
	}
}

func containsAny(s string, phrases []string) bool {
	for _, p := range phrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
