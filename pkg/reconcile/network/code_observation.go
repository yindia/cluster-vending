package network

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/yindia/cluster-vending/pkg/kube"
)

// providerObservationBound is how long after the platform last changed an
// object its provider has surely observed it: Crossplane reconciles a managed
// resource on every spec change it watches and again every poll interval
// (10m by default for the AWS providers), plus margin.
const providerObservationBound = 15 * time.Minute

// observedSinceApply reports whether the conditions of an object whose
// provider stamps no observedGeneration on them (provider-aws v2.9.0:
// Condition.Equal keeps a condition written without one for as long as its
// status, reason and message stay the same) can be from before the
// platform's last change of it. It reads managedFields, all apiserver times:
// the platform's last change is its field managers' latest entry; a status
// write by another manager (the provider) after that observed the new spec,
// since Crossplane's status updates carry the resourceVersion it read. With
// no such write, the change counts as observed once it is older than
// providerObservationBound. Otherwise why says what it waits for.
func observedSinceApply(object *unstructured.Unstructured, now time.Time) (bool, string) {
	var applied, written time.Time
	for _, entry := range object.GetManagedFields() {
		if entry.Time == nil {
			continue
		}
		at := entry.Time.Time
		switch {
		case strings.HasPrefix(entry.Manager, kube.FieldOwner):
			if at.After(applied) {
				applied = at
			}
		case entry.Subresource == "status" && at.After(written):
			written = at
		}
	}
	switch {
	case applied.IsZero():
		return false, "no apply by the platform is recorded on it and its provider publishes no observedGeneration"
	// Strictly after: managedFields times have second resolution, so a
	// write in the apply's second may have come before it.
	case written.After(applied), now.Sub(applied) >= providerObservationBound:
		return true, ""
	}
	return false, fmt.Sprintf("its provider publishes no observedGeneration and has not written its status since the platform applied it at %s; waiting for that observation (at most %s)",
		applied.UTC().Format(time.RFC3339), providerObservationBound)
}
