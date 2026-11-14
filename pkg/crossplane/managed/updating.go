package managed

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// versionFields are, per EKS kind, the spec.forProvider field of the
// version it is asked to run and the status.atProvider field of the one
// the provider last saw, and whether the two compare by Kubernetes minor
// (the cluster and node groups: EKS reports "1.37" or "1.37.2") or as they
// are (add-ons: "v1.20.0-eksbuild.1").
var versionFields = map[string]struct {
	field string
	minor bool
}{
	"Cluster":   {"version", true},
	"NodeGroup": {"version", true},
	"Addon":     {"addonVersion", false},
}

// updating reports whether a ready EKS object is being moved to another
// version: the provider reports another version than its spec asks for
// (a Kubernetes upgrade EKS runs, which Ready=True does not show), or EKS
// reports it UPDATING. message says from what to what. The provider sees
// the cloud at its poll interval, so the move shows until its next look.
func updating(u *unstructured.Unstructured) (message string, moving bool) {
	if u.GetKind() == "Release" && strings.HasPrefix(u.GroupVersionKind().Group, "helm.") {
		return helmUpdating(u)
	}
	f, ok := versionFields[u.GetKind()]
	if !ok || !strings.HasPrefix(u.GroupVersionKind().Group, "eks.") {
		return "", false
	}
	want, _, _ := unstructured.NestedString(u.Object, "spec", "forProvider", f.field)
	have, _, _ := unstructured.NestedString(u.Object, "status", "atProvider", f.field)
	if want != "" && have != "" && !sameVersion(want, have, f.minor) {
		return fmt.Sprintf("updating: %s → %s", have, want), true
	}
	if status, _, _ := unstructured.NestedString(u.Object, "status", "atProvider", "status"); status == "UPDATING" {
		return "updating", true
	}
	return "", false
}

// helmUpdating reports whether Helm is applying a new chart version or
// new values to a ready Release (a platform component): provider-helm
// reports no chart version, so it is the provider not having taken the
// latest spec yet (its Synced condition is for an older generation; a
// provider that writes no observed generation is not judged), or Helm in
// a pending state (pending-install, pending-upgrade, pending-rollback).
func helmUpdating(u *unstructured.Unstructured) (string, bool) {
	if state, _, _ := unstructured.NestedString(u.Object, "status", "atProvider", "state"); strings.HasPrefix(state, "pending-") {
		return "updating: Helm is " + state, true
	}
	synced := conditions(u)["Synced"]
	if synced.observedGeneration > 0 && synced.observedGeneration < u.GetGeneration() {
		chart, _, _ := unstructured.NestedString(u.Object, "spec", "forProvider", "chart", "version")
		return fmt.Sprintf("updating: applying chart %s with new values", chart), true
	}
	return "", false
}

func sameVersion(want, have string, minor bool) bool {
	if !minor {
		return want == have
	}
	return minorOf(want) == minorOf(have)
}

// minorOf is "1.37" of "1.37" and of "1.37.2".
func minorOf(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}
