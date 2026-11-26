package helmrel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// DigestAnnotation is where the digest of a Release's spec is kept on the
// Release object: the spec is written again only when it differs.
const DigestAnnotation = "cloud.cluster-vending.io/helm-digest"

// The platform's markers on a Release it paused or gave up on. The pause
// itself is Crossplane's crossplane.io/paused; the markers say it is the
// platform's backoff, so an operator's or a freeze's pause is never lifted
// by mistake.
const (
	// BackoffAnnotation holds the time (RFC 3339) the backoff ends.
	BackoffAnnotation = "cloud.cluster-vending.io/helm-backoff-until"
	// GaveUpAnnotation is "true" on a Release the platform gave up on: it
	// no longer holds back the cluster's other Helm operations.
	GaveUpAnnotation = "cloud.cluster-vending.io/helm-gave-up"
)

// Spec is what makes a Release the one it is: the chart, the values and
// the settings that change how it is installed.
type Spec struct {
	Repository, Chart, Version string
	// Values are the final, rendered values.
	Values   map[string]any
	SkipCRDs bool
	// Timeout is the install timeout: a longer one must reach the live
	// Release, so it is part of the digest.
	Timeout time.Duration
	// Wait is spec.forProvider.wait: whether Helm itself waits for the
	// workloads. The platform renders false (it judges the rollout itself,
	// docs/architecture.md#helm-operations); part of the digest so a
	// Release an older build wrote with wait: true is written again.
	Wait bool
}

// Digest is the hex sha256 of the spec in a canonical form (encoding/json
// writes map keys sorted): two renders of the same chart, version and
// values have the same digest.
func Digest(s Spec) string {
	values := s.Values
	if values == nil {
		values = map[string]any{}
	}
	raw, err := json.Marshal(struct {
		Repository string         `json:"repository"`
		Chart      string         `json:"chart"`
		Version    string         `json:"version"`
		Values     map[string]any `json:"values"`
		SkipCRDs   bool           `json:"skipCRDs"`
		Timeout    string         `json:"timeout"`
		Wait       bool           `json:"wait"`
	}{s.Repository, s.Chart, s.Version, values, s.SkipCRDs, s.Timeout.String(), s.Wait})
	if err != nil {
		// Values come from JSON or YAML; one that cannot be encoded has no
		// stable digest: a unique one makes the Release be written.
		return "unencodable-" + err.Error()
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
