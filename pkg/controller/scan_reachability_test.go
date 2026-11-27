package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// The three scanners run Deps.ScanWorkers clusters at once; unset, one.
func TestScanWorkers(t *testing.T) {
	for _, tc := range []struct{ workers, want int }{{0, 1}, {-1, 1}, {1, 1}, {4, 4}} {
		if got := scanOptions(Deps{ScanWorkers: tc.workers}).MaxConcurrentReconciles; got != tc.want {
			t.Errorf("ScanWorkers %d: MaxConcurrentReconciles = %d, want %d", tc.workers, got, tc.want)
		}
	}
}

func unreachable(obj *cloudv1alpha1.Cluster) {
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{Type: cloudv1alpha1.ConditionReachable,
		Status: metav1.ConditionFalse, Reason: "Unreachable"})
}

// fakeProbes is a gate whose probe answers with *answer and counts calls.
func fakeProbes(answer *error, now *time.Time) (*ReachabilityProbes, *int) {
	calls := 0
	p := NewReachabilityProbes()
	p.probe = func(context.Context, []byte) error { calls++; return *answer }
	p.now = func() time.Time { return *now }
	return p, &calls
}

func TestAnUnreachableClusterIsProbedBeforeAScan(t *testing.T) {
	kubeconfig := func(context.Context) ([]byte, error) { return []byte("kubeconfig"), nil }
	now := time.Unix(1_800_000_000, 0)
	answer := errors.New("dial tcp: i/o timeout")
	p, calls := fakeProbes(&answer, &now)
	obj := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{UID: "prod-uid"}}

	// Reachable (or not known yet): scanned, nothing probed.
	if wait, skip := p.Skip(t.Context(), obj, kubeconfig); skip || wait != 0 || *calls != 0 {
		t.Fatalf("a reachable cluster: skip %t after %v, %d probes", skip, wait, *calls)
	}

	unreachable(obj)
	wait, skip := p.Skip(t.Context(), obj, kubeconfig)
	if !skip || wait != ReachabilityProbeInterval || *calls != 1 {
		t.Fatalf("unreachable, probe fails: skip %t after %v, %d probes", skip, wait, *calls)
	}

	// Within the interval: skipped without another probe.
	now = now.Add(time.Minute)
	wait, skip = p.Skip(t.Context(), obj, kubeconfig)
	if !skip || wait != ReachabilityProbeInterval-time.Minute || *calls != 1 {
		t.Fatalf("within the interval: skip %t after %v, %d probes", skip, wait, *calls)
	}

	// Past it the probe runs again, and its success lets the scan run.
	now = now.Add(ReachabilityProbeInterval)
	answer = nil
	if _, skip := p.Skip(t.Context(), obj, kubeconfig); skip || *calls != 2 {
		t.Fatalf("the probe succeeded: skip %t, %d probes", skip, *calls)
	}
	// The other scanners take that success without probing again.
	if _, skip := p.Skip(t.Context(), obj, kubeconfig); skip || *calls != 2 {
		t.Fatalf("a recent success: skip %t, %d probes", skip, *calls)
	}
}

// A kubeconfig that cannot be read is a failed probe.
func TestAProbeWithoutAKubeconfigFails(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var answer error
	p, calls := fakeProbes(&answer, &now)
	obj := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{UID: "prod-uid"}}
	unreachable(obj)

	_, skip := p.Skip(t.Context(), obj, func(context.Context) ([]byte, error) { return nil, errors.New("secret not found") })

	if !skip || *calls != 0 {
		t.Errorf("skip %t, %d probes: want skipped without a probe", skip, *calls)
	}
}

// A nil gate never skips (reconcilers built without one).
func TestNoReachabilityGate(t *testing.T) {
	var p *ReachabilityProbes
	obj := &cloudv1alpha1.Cluster{}
	unreachable(obj)
	if _, skip := p.Skip(t.Context(), obj, nil); skip {
		t.Error("a nil gate skipped")
	}
}

// The deprecation scan of an unreachable cluster waits for the probe: the
// workload cluster is not read and nothing is recorded.
func TestDeprecationScanSkipsAnUnreachableCluster(t *testing.T) {
	w, r, connected := scanWorld(t)
	now := time.Unix(1_800_000_000, 0)
	answer := errors.New("dial tcp: i/o timeout")
	r.deps.Reachability, _ = fakeProbes(&answer, &now)
	obj := w.cluster(t, "prod")
	unreachable(obj)
	w.must(t, w.k.Status().Update(t.Context(), obj))

	res, err := r.Reconcile(t.Context(), request("prod"))

	if err != nil || res.RequeueAfter != ReachabilityProbeInterval || len(*connected) != 0 || w.cluster(t, "prod").Status.Deprecations != nil {
		t.Fatalf("Reconcile = %+v, %v; %d connections", res, err, len(*connected))
	}
}
