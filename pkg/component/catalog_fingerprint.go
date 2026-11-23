package component

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"
)

// UnknownFingerprint is the catalog fingerprint of an entry that cannot be
// computed this pass: a placeholder of its values has no value yet, or
// they cannot be expanded. A pass that cannot compute it keeps the one
// recorded, so a value that is briefly unreadable is no catalog change.
const UnknownFingerprint = "?"

// fingerprintInput is what a platform definition renders for one cluster
// that a catalog change is about (PROP-01 4.7): its values with the
// placeholders resolved, its charts, its install timeout, what it
// requires and where it installs. The chart versions are the release's
// pins, not the definition's: a release move or a Kubernetes upgrade
// changes them, gated by its own rollout. The sequence is not part of it
// (it orders, it renders nothing), and neither is the component's IAM.
type fingerprintInput struct {
	Namespace  string        `json:"namespace"`
	Main       Chart         `json:"main"`
	CRDs       *Chart        `json:"crds,omitempty"`
	Companions []Companion   `json:"companions,omitempty"`
	Timeout    time.Duration `json:"timeout"`
	Requires   []string      `json:"requires,omitempty"`
	Values     any           `json:"values"`
}

// CatalogFingerprint is the fingerprint of a platform definition on one
// cluster, given its placeholder values: it changes when what the
// definition renders changes. UnknownFingerprint when its values cannot
// be expanded now.
func CatalogFingerprint(e PlatformEntry, vars Vars) string {
	values := Values{}
	if e.PlatformValues != nil {
		values = mergeInto(values, e.PlatformValues)
	}
	expanded, err := Expand(values, vars)
	if err != nil {
		return UnknownFingerprint
	}
	requires := slices.Clone(e.Requires)
	slices.Sort(requires)
	return Fingerprint(fingerprintInput{Namespace: e.Install.Namespace, Main: e.Install.Main, CRDs: e.Install.CRDs,
		Companions: e.Install.Companions, Timeout: e.Install.Timeout, Requires: requires, Values: expanded})
}

// Fingerprint is a short digest of v's JSON (map keys sorted, so equal
// values give equal digests); UnknownFingerprint when v cannot be encoded.
func Fingerprint(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return UnknownFingerprint
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}
