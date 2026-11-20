package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudapi "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/policy"
)

func TestPolicyControllerSnapshotAndCadence(t *testing.T) {
	w, _, _ := scanWorld(t)
	r := NewPolicyReconciler(w.deps)
	calls := 0
	r.collect = func(ctx context.Context, k []byte, now time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
		calls++
		if string(k) != "kubeconfig of prod" {
			t.Fatal("wrong credentials")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing timeout")
		}
		return &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamppb.New(now)}, Violations: []*cloudv1.ClusterViolation{{Id: "a", Result: "error"}}}, nil
	}
	result, err := r.Reconcile(t.Context(), request("prod"))
	if err != nil || result.RequeueAfter != policy.Interval || calls != 1 {
		t.Fatalf("%v %v calls=%d", result, err, calls)
	}
	stored := policy.Decode([]byte(w.cluster(t, "prod").Status.PolicySnapshot))
	if len(stored.Violations) != 1 {
		t.Fatal(stored)
	}
	_, err = r.Reconcile(t.Context(), request("prod"))
	if err != nil || calls != 1 {
		t.Fatal("unbounded polling", err, calls)
	}
}
func TestPolicyControllerRefusesCrossNamespaceAndUnscannable(t *testing.T) {
	for _, scenario := range []string{"other-secret", "pending", "paused"} {
		t.Run(scenario, func(t *testing.T) {
			w, _, _ := scanWorld(t)
			r := NewPolicyReconciler(w.deps)
			calls := 0
			r.collect = func(context.Context, []byte, time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
				calls++
				return nil, errors.New("unexpected")
			}
			c := w.cluster(t, "prod")
			switch scenario {
			case "other-secret":
				c.Status.KubeconfigSecret = "tenant-other/secret"
			case "pending":
				c.Status.Phase = cloudapi.ClusterPhaseProvisioning
			case "paused":
				w.pauseTenant(t, operatorPause)
			}
			if scenario != "paused" {
				w.must(t, w.k.Status().Update(t.Context(), c))
			}
			_, err := r.Reconcile(t.Context(), request("prod"))
			if err != nil || calls != 0 {
				t.Fatal(err, calls)
			}
			snapshot := w.cluster(t, "prod").Status.PolicySnapshot
			if scenario == "other-secret" {
				if decoded := policy.Decode([]byte(snapshot)); !decoded.Summary.Incomplete || decoded.Summary.CollectedAt != nil {
					t.Fatal("missing access notice")
				}
			} else if snapshot != "" {
				t.Fatal("unscannable cluster written")
			}
		})
	}
}
func TestPolicyControllerCancellationAndNilResult(t *testing.T) {
	for _, scenario := range []string{"cancel", "nil"} {
		t.Run(scenario, func(t *testing.T) {
			w, _, _ := scanWorld(t)
			r := NewPolicyReconciler(w.deps)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r.collect = func(context.Context, []byte, time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
				if scenario == "cancel" {
					cancel()
					return nil, context.Canceled
				}
				return nil, nil
			}
			_, err := r.Reconcile(ctx, request("prod"))
			raw := w.cluster(t, "prod").Status.PolicySnapshot
			if scenario == "cancel" {
				if !errors.Is(err, context.Canceled) || raw != "" {
					t.Fatal("cancellation wrote snapshot", err, raw)
				}
			} else if err != nil || !policy.Decode([]byte(raw)).Summary.Incomplete || policy.Decode([]byte(raw)).Summary.CollectedAt != nil {
				t.Fatal("nil collector", err, raw)
			}
		})
	}
}
func TestPolicyControllerDoesNotClobberConcurrentStatusOrRecreatedObject(t *testing.T) {
	for _, scenario := range []string{"concurrent", "recreated"} {
		t.Run(scenario, func(t *testing.T) {
			w, _, _ := scanWorld(t)
			r := NewPolicyReconciler(w.deps)
			r.collect = func(ctx context.Context, _ []byte, at time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
				c := w.cluster(t, "prod")
				if scenario == "recreated" {
					w.must(t, w.k.Delete(ctx, c))
					replacement := &cloudapi.Cluster{ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace, UID: "recreated-uid"}, Spec: c.Spec}
					w.must(t, w.k.Create(ctx, replacement))
				} else {
					// Another status writer (the cluster reconciler) lands during the scan.
					c.Status.CurrentKubernetesVersion = "1.36.0"
					w.must(t, w.k.Status().Update(ctx, c))
				}
				return &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamppb.New(at)}}, nil
			}
			_, err := r.Reconcile(t.Context(), request("prod"))
			c := &cloudapi.Cluster{}
			w.must(t, w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: "prod"}, c))
			if scenario == "concurrent" {
				if err != nil || c.Status.CurrentKubernetesVersion != "1.36.0" || c.Status.PolicySnapshot == "" {
					t.Fatalf("scan lost or clobbered status: err=%v version=%q snapshot=%q", err, c.Status.CurrentKubernetesVersion, c.Status.PolicySnapshot)
				}
				return
			}
			if !apierrors.IsConflict(err) || c.Status.PolicySnapshot != "" {
				t.Fatalf("wrote onto a recreated object: err=%v snapshot=%q", err, c.Status.PolicySnapshot)
			}
		})
	}
}
func TestPolicyControllerKeepsPreviousSnapshotOnFailure(t *testing.T) {
	for _, scenario := range []string{"collect-error", "kubeconfig-error"} {
		t.Run(scenario, func(t *testing.T) {
			w, _, _ := scanWorld(t)
			r := NewPolicyReconciler(w.deps)
			old := time.Now().Add(-2 * policy.Interval)
			prev := &cloudv1.ClusterPolicySnapshot{
				Summary:    &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamppb.New(old)},
				Violations: []*cloudv1.ClusterViolation{{Id: "keep", Result: "fail"}},
				Events:     []*cloudv1.ClusterViolationEvent{{Id: "ev", LastObservedAt: timestamppb.New(time.Now())}},
			}
			raw, err := policy.Encode(prev)
			w.must(t, err)
			c := w.cluster(t, "prod")
			c.Status.PolicySnapshot = string(raw)
			if scenario == "kubeconfig-error" {
				c.Status.KubeconfigSecret = "tenant-other/secret"
			}
			w.must(t, w.k.Status().Update(t.Context(), c))
			r.collect = func(context.Context, []byte, time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
				return nil, errors.New("boom")
			}
			for range 2 { // repeated failure must not pile up notices
				if _, err := r.Reconcile(t.Context(), request("prod")); err != nil {
					t.Fatal(err)
				}
			}
			got := policy.Decode([]byte(w.cluster(t, "prod").Status.PolicySnapshot))
			if len(got.Violations) != 1 || len(got.Events) != 1 || !got.Summary.Incomplete {
				t.Fatalf("previous findings lost: %v", got)
			}
			if got.Summary.CollectedAt == nil || !got.Summary.CollectedAt.AsTime().Equal(old.Truncate(time.Microsecond)) && got.Summary.CollectedAt.AsTime().Sub(old).Abs() > time.Second {
				t.Fatalf("collectedAt not kept: %v", got.Summary.CollectedAt)
			}
			failed := 0
			for _, n := range got.Summary.Notices {
				if strings.Contains(n, "latest") && strings.Contains(n, "failed") {
					failed++
				}
			}
			if failed != 1 {
				t.Fatalf("want one latest-collection-failed notice: %v", got.Summary.Notices)
			}
		})
	}
}
func TestScannersSkipPausedCluster(t *testing.T) {
	w, d, connected := scanWorld(t)
	c := w.cluster(t, "prod")
	c.Annotations = map[string]string{managed.PausedAnnotation: "true"}
	w.must(t, w.k.Update(t.Context(), c))
	p := NewPolicyReconciler(w.deps)
	pCalls := 0
	p.collect = func(context.Context, []byte, time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
		pCalls++
		return nil, errors.New("unexpected")
	}
	rec := NewRecommendationReconciler(w.deps)
	rec.enabled = true
	for name, run := range map[string]func() (ctrl.Result, error){
		"policy":         func() (ctrl.Result, error) { return p.Reconcile(t.Context(), request("prod")) },
		"deprecation":    func() (ctrl.Result, error) { return d.Reconcile(t.Context(), request("prod")) },
		"recommendation": func() (ctrl.Result, error) { return rec.Reconcile(t.Context(), request("prod")) },
	} {
		result, err := run()
		if err != nil || result.RequeueAfter != notScannableDelay {
			t.Errorf("%s: %v %v", name, result, err)
		}
	}
	if pCalls != 0 || len(*connected) != 0 {
		t.Fatalf("paused cluster was read: policy=%d connects=%v", pCalls, *connected)
	}
}

// Collection only reads the workload cluster, so it also runs while the
// cluster is Failed or Updating (a stuck resource or a change in progress
// must not hide its policy violations), as long as its kubeconfig exists.
func TestPolicyControllerScansFailedAndUpdatingClusters(t *testing.T) {
	for _, phase := range []cloudapi.ClusterPhase{cloudapi.ClusterPhaseFailed, cloudapi.ClusterPhaseUpdating} {
		t.Run(string(phase), func(t *testing.T) {
			w, _, _ := scanWorld(t)
			r := NewPolicyReconciler(w.deps)
			calls := 0
			r.collect = func(_ context.Context, _ []byte, now time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
				calls++
				return &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamppb.New(now)}}, nil
			}
			c := w.cluster(t, "prod")
			c.Status.Phase = phase
			w.must(t, w.k.Status().Update(t.Context(), c))

			if _, err := r.Reconcile(t.Context(), request("prod")); err != nil || calls != 1 {
				t.Fatalf("%s cluster: err %v, %d collections; want 1", phase, err, calls)
			}
		})
	}
}
