package helmrel_test

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/helmrel"
)

// The messages are the ones provider-helm reported on a live cluster.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		want          helmrel.ErrorClass
	}{
		{"capsule: workload API unreachable",
			`update failed: failed to upgrade release: could not get server version from Kubernetes: Get "https://ABC.gr7.eu-west-1.eks.amazonaws.com/version?timeout=32s": net/http: TLS handshake timeout`,
			helmrel.ClassTransient},
		{"capsule: version timeout without a reason", `could not get server version from Kubernetes: Get "https://x/version?timeout=32s": context deadline exceeded`, helmrel.ClassTransient},
		{"connection refused", `Get "https://x/api": dial tcp 10.0.0.1:443: connect: connection refused`, helmrel.ClassTransient},
		{"i/o timeout", `dial tcp 10.0.0.1:443: i/o timeout`, helmrel.ClassTransient},
		{"TLS handshake timeout", `net/http: TLS handshake timeout`, helmrel.ClassTransient},
		{"throttled", `the server has received too many requests and has asked us to try again later`, helmrel.ClassTransient},
		{"API unavailable", `the server is currently unable to handle the request`, helmrel.ClassTransient},
		{"no such host", `dial tcp: lookup x.eks.amazonaws.com: no such host`, helmrel.ClassTransient},
		{"kyverno: lock left behind",
			`update failed: failed to upgrade release: another operation (install/upgrade/rollback) is in progress`, helmrel.ClassTransient},
		{"kyverno: uninstall never finished", `update failed: failed to upgrade release: "kyverno" has no deployed releases`, helmrel.ClassTransient},
		{"gpu-operator: hook did not finish",
			`update failed: failed to upgrade release: pre-upgrade hooks failed: resource Job/gpu-operator/gpu-operator-upgrade-crd not ready, status: InProgress, message: Job in progress`,
			helmrel.ClassRelease},
		{"rollout timeout", `update failed: failed to upgrade release: resource Deployment/kyverno/kyverno-admission-controller not ready. status: InProgress, message: Available: 0/3: context deadline exceeded`, helmrel.ClassRelease},
		{"invalid values", `install failed: values don't meet the specifications of the schema(s) in the following chart(s)`, helmrel.ClassRelease},
		{"chart not found", `cannot re-install: failed to locate chart: chart "kyverno" version "9.9.9" not found in https://kyverno.github.io/kyverno/ repository`, helmrel.ClassChart},
		{"pull failed", `failed to pull chart: failed to download "oci://x/y" at version "1.0": manifest unknown`, helmrel.ClassChart},
		{"unknown stays a release error", `something nobody has seen`, helmrel.ClassRelease},
		{"empty", ``, helmrel.ClassNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := helmrel.Classify(tc.message); got != tc.want {
				t.Errorf("Classify = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTransientNeverCounts(t *testing.T) {
	if helmrel.ClassTransient.Counts() || helmrel.ClassChart.Counts() || helmrel.ClassNone.Counts() {
		t.Error("only a release error counts toward giving up")
	}
	if !helmrel.ClassRelease.Counts() {
		t.Error("a release error counts")
	}
}
