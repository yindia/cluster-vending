package helmrel_test

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/yindia/cluster-vending/pkg/helmrel"
)

func owned(name, release string, generation int64) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: "kyverno", Generation: generation, Annotations: map[string]string{
		helmrel.ReleaseNameAnnotation: release, helmrel.ReleaseNamespaceAnnotation: "kyverno"}}
}

func deployment(name string, generation, observed int64, desired, updated, available int32) appsv1.Deployment {
	return appsv1.Deployment{ObjectMeta: owned(name, "kyverno", generation), Spec: appsv1.DeploymentSpec{Replicas: ptr.To(desired)},
		Status: appsv1.DeploymentStatus{ObservedGeneration: observed, UpdatedReplicas: updated, AvailableReplicas: available}}
}

func TestUnavailable(t *testing.T) {
	ds := func(desired, updated, available int32) appsv1.DaemonSet {
		return appsv1.DaemonSet{ObjectMeta: owned("kyverno-agent", "kyverno", 2), Status: appsv1.DaemonSetStatus{ObservedGeneration: 2,
			DesiredNumberScheduled: desired, UpdatedNumberScheduled: updated, NumberAvailable: available}}
	}
	sts := appsv1.StatefulSet{ObjectMeta: owned("kyverno-store", "kyverno", 1), Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(2))},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, UpdatedReplicas: 2, AvailableReplicas: 1}}
	other := deployment("someone-else", 1, 1, 3, 0, 0)
	other.Annotations[helmrel.ReleaseNameAnnotation] = "other"
	elsewhere := deployment("same-name-other-ns", 1, 1, 3, 0, 0)
	elsewhere.Annotations[helmrel.ReleaseNamespaceAnnotation] = "default"
	for _, tt := range []struct {
		name string
		w    helmrel.Workloads
		want string // "" available
	}{
		{"deployed and available", helmrel.Workloads{Deployments: []appsv1.Deployment{deployment("kyverno-admission-controller", 3, 3, 3, 3, 3)}}, ""},
		{"rolling", helmrel.Workloads{Deployments: []appsv1.Deployment{deployment("kyverno-admission-controller", 3, 3, 3, 3, 1)}},
			"deployment/kyverno-admission-controller: 1/3 available"},
		{"rolling, not all updated", helmrel.Workloads{Deployments: []appsv1.Deployment{deployment("kyverno-admission-controller", 3, 3, 3, 1, 3)}},
			"deployment/kyverno-admission-controller: 1/3 updated, 3/3 available"},
		{"no pods yet", helmrel.Workloads{Deployments: []appsv1.Deployment{deployment("kyverno-admission-controller", 1, 1, 3, 0, 0)}},
			"deployment/kyverno-admission-controller: 0/3 updated, 0/3 available"},
		{"stale observedGeneration", helmrel.Workloads{Deployments: []appsv1.Deployment{deployment("kyverno-admission-controller", 4, 3, 3, 3, 3)}},
			"deployment/kyverno-admission-controller: rollout not observed yet (generation 4, observed 3)"},
		{"a DaemonSet with 0 desired", helmrel.Workloads{DaemonSets: []appsv1.DaemonSet{ds(0, 0, 0)}}, ""},
		{"a DaemonSet rolling", helmrel.Workloads{DaemonSets: []appsv1.DaemonSet{ds(4, 4, 3)}}, "daemonset/kyverno-agent: 3/4 available"},
		{"a StatefulSet short of available", helmrel.Workloads{StatefulSets: []appsv1.StatefulSet{sts}}, "statefulset/kyverno-store: 1/2 available"},
		{"a chart with no workloads", helmrel.Workloads{}, ""},
		{"other releases' workloads do not count", helmrel.Workloads{Deployments: []appsv1.Deployment{other, elsewhere}}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := helmrel.Unavailable(tt.w, "kyverno", "kyverno"); got != tt.want {
				t.Errorf("Unavailable = %q, want %q", got, tt.want)
			}
		})
	}
}

// The first workload that is not available is named, in a stable order.
func TestUnavailableNamesTheFirstInOrder(t *testing.T) {
	w := helmrel.Workloads{Deployments: []appsv1.Deployment{deployment("b", 1, 1, 1, 1, 0), deployment("a", 1, 1, 1, 1, 0)}}
	if got := helmrel.Unavailable(w, "kyverno", "kyverno"); !strings.HasPrefix(got, "deployment/a:") {
		t.Errorf("Unavailable = %q, want deployment/a first", got)
	}
}

func TestRolloutMarkRoundTrip(t *testing.T) {
	since := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	for _, m := range []helmrel.RolloutMark{
		{Digest: "abc", Revision: 3, Since: since},
		{Digest: "abc", Revision: 4, Done: true},
	} {
		got, ok := helmrel.ParseRolloutMark(m.String())
		if !ok || got != m {
			t.Errorf("ParseRolloutMark(%q) = %+v, %v; want %+v", m.String(), got, ok, m)
		}
	}
	for _, bad := range []string{"", "abc", "abc/x/ready", "abc/1/yesterday"} {
		if _, ok := helmrel.ParseRolloutMark(bad); ok {
			t.Errorf("ParseRolloutMark(%q) read a mark", bad)
		}
	}
	m := helmrel.RolloutMark{Digest: "abc", Revision: 3, Since: since}
	if !m.About("abc", 3) || m.About("abd", 3) || m.About("abc", 4) {
		t.Error("About must match digest and revision")
	}
}

// A rollout that timed out is a counted release error, whatever the
// workload message says.
func TestRolloutTimeoutIsAReleaseError(t *testing.T) {
	msg := helmrel.RolloutTimedOut(10*time.Minute, "deployment/x: could not get server version")
	if got := helmrel.Classify(msg); got != helmrel.ClassRelease {
		t.Errorf("Classify(%q) = %s, want release", msg, got)
	}
	if !strings.HasPrefix(msg, "rollout timed out after 10m0s") {
		t.Errorf("message %q", msg)
	}
}
