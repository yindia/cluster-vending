package helmrel

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Helm's ownership annotations on every object it installs (helm.sh/helm
// pkg/action/validate.go): which release, in which namespace, the object
// belongs to.
const (
	ReleaseNameAnnotation      = "meta.helm.sh/release-name"
	ReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
)

// Workloads are the Deployments, StatefulSets and DaemonSets of one
// namespace of a workload cluster: what a release's rollout is judged by.
type Workloads struct {
	Deployments  []appsv1.Deployment
	StatefulSets []appsv1.StatefulSet
	DaemonSets   []appsv1.DaemonSet
}

// Unavailable says which workload of release (installed into namespace)
// is not available yet, and why: "deployment/x: 1/3 available". "" when
// all of them are, or the release has none (a chart of CRDs or policies:
// it is ready once Helm deployed it). A workload is the release's when
// Helm's ownership annotations name it.
//
// Available, as Helm's own wait judged it: the controller observed the
// latest spec (observedGeneration >= generation) and the desired replicas
// are updated and available. A DaemonSet that should run nowhere (0
// desired) is available. Workloads are looked at by kind, then name, so
// the same one is named pass after pass.
func Unavailable(w Workloads, release, namespace string) string {
	var out []string
	for _, d := range sortedByName(w.Deployments, func(d appsv1.Deployment) metav1.ObjectMeta { return d.ObjectMeta }) {
		if ownedBy(d.ObjectMeta, release, namespace) {
			out = append(out, deploymentProblem(d))
		}
	}
	for _, s := range sortedByName(w.StatefulSets, func(s appsv1.StatefulSet) metav1.ObjectMeta { return s.ObjectMeta }) {
		if ownedBy(s.ObjectMeta, release, namespace) {
			out = append(out, statefulSetProblem(s))
		}
	}
	for _, d := range sortedByName(w.DaemonSets, func(d appsv1.DaemonSet) metav1.ObjectMeta { return d.ObjectMeta }) {
		if ownedBy(d.ObjectMeta, release, namespace) {
			out = append(out, daemonSetProblem(d))
		}
	}
	for _, p := range out {
		if p != "" {
			return p
		}
	}
	return ""
}

func sortedByName[T any](items []T, meta func(T) metav1.ObjectMeta) []T {
	out := slices.Clone(items)
	slices.SortFunc(out, func(a, b T) int { return cmp.Compare(meta(a).Name, meta(b).Name) })
	return out
}

// ownedBy reports whether Helm's annotations make the object release's: its
// name, and its namespace when the object says one.
func ownedBy(m metav1.ObjectMeta, release, namespace string) bool {
	if m.Annotations[ReleaseNameAnnotation] != release {
		return false
	}
	ns, ok := m.Annotations[ReleaseNamespaceAnnotation]
	return !ok || ns == namespace
}

func stale(kind string, m metav1.ObjectMeta, observed int64) string {
	if observed >= m.Generation {
		return ""
	}
	return fmt.Sprintf("%s/%s: rollout not observed yet (generation %d, observed %d)", kind, m.Name, m.Generation, observed)
}

// counts is the problem of a workload with updated and available of
// desired; "" when both reach it.
func counts(kind, name string, desired, updated, available int32) string {
	switch {
	case updated < desired:
		return fmt.Sprintf("%s/%s: %d/%d updated, %d/%d available", kind, name, updated, desired, available, desired)
	case available < desired:
		return fmt.Sprintf("%s/%s: %d/%d available", kind, name, available, desired)
	}
	return ""
}

func replicas(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}

func deploymentProblem(d appsv1.Deployment) string {
	if p := stale("deployment", d.ObjectMeta, d.Status.ObservedGeneration); p != "" {
		return p
	}
	if d.Spec.Paused {
		return "" // Helm's wait does not wait for a paused Deployment either
	}
	return counts("deployment", d.Name, replicas(d.Spec.Replicas), d.Status.UpdatedReplicas, d.Status.AvailableReplicas)
}

func statefulSetProblem(s appsv1.StatefulSet) string {
	if p := stale("statefulset", s.ObjectMeta, s.Status.ObservedGeneration); p != "" {
		return p
	}
	desired := replicas(s.Spec.Replicas)
	updated := s.Status.UpdatedReplicas
	switch {
	case s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType:
		updated = desired // pods are replaced only when someone deletes them
	case s.Spec.UpdateStrategy.RollingUpdate != nil && s.Spec.UpdateStrategy.RollingUpdate.Partition != nil:
		updated += min(*s.Spec.UpdateStrategy.RollingUpdate.Partition, desired) // the pods below the partition keep the old spec
	}
	return counts("statefulset", s.Name, desired, updated, s.Status.AvailableReplicas)
}

func daemonSetProblem(d appsv1.DaemonSet) string {
	if p := stale("daemonset", d.ObjectMeta, d.Status.ObservedGeneration); p != "" {
		return p
	}
	desired := d.Status.DesiredNumberScheduled
	updated := d.Status.UpdatedNumberScheduled
	if d.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType {
		updated = desired
	}
	return counts("daemonset", d.Name, desired, updated, d.Status.NumberAvailable)
}

// RolloutAnnotation is the platform's note on a Release about the rollout
// of its current spec (RolloutMark): since when it has been rolling out, or
// that it rolled out. It is what makes the install timeout run on the
// platform's side, across passes and restarts, and what lets a Release
// that rolled out stay Ready without its workloads being read again.
const RolloutAnnotation = "cloud.cluster-vending.io/helm-rollout"

// RolloutMark is a RolloutAnnotation: the Release's digest and Helm
// revision it is about, and either since when that revision has been
// rolling out or that it is done (its workloads were available once).
type RolloutMark struct {
	Digest   string
	Revision int32
	Since    time.Time
	Done     bool
}

const rolloutDone = "ready"

// String is the mark as the annotation holds it: "<digest>/<revision>/<RFC
// 3339 since>" or "<digest>/<revision>/ready".
func (m RolloutMark) String() string {
	at := rolloutDone
	if !m.Done {
		at = m.Since.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s/%d/%s", m.Digest, m.Revision, at)
}

// ParseRolloutMark reads an annotation's value; false when it is no mark.
func ParseRolloutMark(s string) (RolloutMark, bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 || parts[0] == "" {
		return RolloutMark{}, false
	}
	rev, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil {
		return RolloutMark{}, false
	}
	m := RolloutMark{Digest: parts[0], Revision: int32(rev)}
	if parts[2] == rolloutDone {
		m.Done = true
		return m, true
	}
	if m.Since, err = time.Parse(time.RFC3339, parts[2]); err != nil {
		return RolloutMark{}, false
	}
	return m, true
}

// About reports whether the mark is about the Release's spec digest and
// Helm revision: a new spec or a new revision is a new rollout.
func (m RolloutMark) About(digest string, revision int32) bool {
	return m.Digest == digest && m.Revision == revision
}

// rolloutTimedOutPrefix starts the error of a rollout that did not finish
// within the install timeout.
const rolloutTimedOutPrefix = "rollout timed out after "

// RolloutTimedOut is the error of a release whose workloads were not
// available within timeout of Helm deploying it; workload names the one
// waited for. A release error (Classify): counted toward giving up, as a
// failed Helm wait was.
func RolloutTimedOut(timeout time.Duration, workload string) string {
	return rolloutTimedOutPrefix + timeout.String() + " waiting for " + workload
}
