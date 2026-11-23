package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/naas"
)

// nqWorld is a NaaS cluster with its bootstrap installed, and the
// qualification controller at a fixed time.
type nqWorld struct {
	*stWorld
	r   *NaaSQualificationReconciler
	now time.Time
}

func newQualificationWorld(t *testing.T, image string) *nqWorld {
	t.Helper()
	sw := newSubTenantWorld(t)
	protected := &corev1.Namespace{}
	if err := sw.remote.Get(t.Context(), client.ObjectKey{Name: naas.QualificationNamespace}, protected); err != nil {
		t.Fatal(err)
	}
	protected.UID = "platform-namespace-uid"
	if err := sw.remote.Update(t.Context(), protected); err != nil {
		t.Fatal(err)
	}
	deps := sw.deps
	// The fake assigns no UIDs; the controller records only a Job's own result.
	uids := interceptor.NewClient(sw.remote.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				obj.SetUID(types.UID("uid-" + obj.GetName()))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	deps.SubTenantWorkloads = &stClients{client: uids}
	deps.NaaS = config.NaaS{QualifierImage: image}
	w := &nqWorld{stWorld: sw, now: time.Now().UTC().Truncate(time.Second)}
	w.r = NewNaaSQualificationReconciler(deps)
	w.r.now = func() time.Time { return w.now }
	return w
}

func (w *nqWorld) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	result, err := w.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "tenant-acme", Name: "parent"}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (w *nqWorld) condition(t *testing.T) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(w.parent(t).Status.Conditions, cloud.ConditionNaaSQualified)
}

func (w *nqWorld) jobs(t *testing.T) []batchv1.Job {
	t.Helper()
	jobs := &batchv1.JobList{}
	if err := w.remote.List(t.Context(), jobs, client.InNamespace(naas.QualificationNamespace)); err != nil {
		t.Fatal(err)
	}
	return jobs.Items
}

func (w *nqWorld) key(t *testing.T) naas.ClusterQualificationKey {
	t.Helper()
	request, err := naas.CurrentClusterQualificationRequest(t.Context(), w.management, w.remote, "tenant-acme", "parent")
	if err != nil {
		t.Fatal(err)
	}
	return request.Key
}

// finish marks the Job done and, when checks is not nil, leaves its result.
func (w *nqWorld) finish(t *testing.T, job batchv1.Job, complete bool, checks map[string]bool) {
	t.Helper()
	if checks != nil {
		result := &naas.QualifierResult{Version: naas.QualifierResultVersion, Key: w.key(t), JobUID: string(job.UID), StartedAt: w.now, FinishedAt: w.now, Checks: checks, Failures: map[string]string{}}
		for check, ok := range checks {
			if !ok {
				result.Failures[check] = "probe succeeded: nc 1.1.1.1 443"
			}
		}
		if err := naas.WriteQualifierResult(t.Context(), w.remote, result); err != nil {
			t.Fatal(err)
		}
	}
	kind := batchv1.JobComplete
	if !complete {
		kind = batchv1.JobFailed
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: kind, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"}}
	if err := w.remote.Status().Update(t.Context(), &job); err != nil {
		t.Fatal(err)
	}
}

func allPassed(except ...string) map[string]bool {
	checks := map[string]bool{}
	for _, check := range naas.RequiredQualificationChecks() {
		checks[check] = true
	}
	for _, check := range except {
		checks[check] = false
	}
	return checks
}

func TestNaaSQualificationRunsAndRecordsTheJob(t *testing.T) {
	tests := []struct {
		name     string
		complete bool
		checks   map[string]bool
		status   metav1.ConditionStatus
		reason   string
		text     string
	}{
		{name: "passed run", complete: true, checks: allPassed(), status: metav1.ConditionTrue, reason: reasonQualificationPassed, text: "Passed all 11 checks"},
		{name: "failed check", complete: true, checks: allPassed(naas.CheckEgressDeny), status: metav1.ConditionFalse, reason: reasonQualificationFailed, text: naas.CheckEgressDeny},
		{name: "job failed without a result", complete: false, status: metav1.ConditionFalse, reason: reasonQualificationFailed, text: "DeadlineExceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newQualificationWorld(t, "registry.example/cv:1")
			result := w.reconcile(t)
			jobs := w.jobs(t)
			if len(jobs) != 1 || w.condition(t).Reason != reasonQualificationRunning || result.RequeueAfter != naasQualificationRunningResync {
				t.Fatalf("first pass: %d jobs, %+v, %s", len(jobs), w.condition(t), result.RequeueAfter)
			}
			for _, obj := range []client.Object{&corev1.ServiceAccount{}, &rbacv1.ClusterRole{}, &rbacv1.Role{}} {
				ns := naas.QualificationNamespace
				if _, cluster := obj.(*rbacv1.ClusterRole); cluster {
					ns = ""
				}
				if err := w.remote.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: naas.QualifierName}, obj); err != nil {
					t.Fatalf("%T not applied: %v", obj, err)
				}
			}
			// A running Job is not started twice.
			w.reconcile(t)
			if len(w.jobs(t)) != 1 {
				t.Fatal("a second Job started while one runs")
			}
			w.finish(t, jobs[0], tt.complete, tt.checks)
			w.now = w.now.Add(time.Minute)
			w.reconcile(t)
			c := w.condition(t)
			if c.Status != tt.status || c.Reason != tt.reason || !strings.Contains(c.Message, tt.text) {
				t.Fatalf("NaaSQualified = %+v", c)
			}
			record, err := naas.ReadClusterQualification(t.Context(), w.remote)
			if err != nil {
				t.Fatal(err)
			}
			if record.Job == nil || record.Job.Name != jobs[0].Name || record.Key != w.key(t) {
				t.Fatalf("record = %+v", record)
			}
			// The recorded Job and its result are removed; a failed run waits
			// for its retry rather than looping.
			if len(w.jobs(t)) != 0 {
				t.Fatal("the recorded Job is kept")
			}
			if err := w.remote.Get(t.Context(), client.ObjectKey{Namespace: naas.QualificationNamespace, Name: naas.QualifierResultName}, &corev1.ConfigMap{}); tt.checks != nil && !apierrors.IsNotFound(err) {
				t.Fatalf("the result is kept: %v", err)
			}
		})
	}
}

func TestNaaSQualificationRenewalAndKeyChange(t *testing.T) {
	w := newQualificationWorld(t, "registry.example/cv:1")
	key := w.key(t)
	day := 24 * time.Hour
	write := func(issued time.Time, k naas.ClusterQualificationKey) {
		t.Helper()
		q := &naas.ClusterQualification{Version: naas.ClusterQualificationVersion, Key: k, IssuedAt: issued, ExpiresAt: issued.Add(naas.MaxQualificationValidity), Evidence: "operator run 42", Checks: allPassed()}
		desired, err := naas.ClusterQualificationConfigMap(q)
		if err != nil {
			t.Fatal(err)
		}
		if err := applyConfigMap(t.Context(), w.remote, desired); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh operator record: nothing runs, the cluster is qualified.
	write(w.now.Add(-day), key)
	result := w.reconcile(t)
	if len(w.jobs(t)) != 0 || w.condition(t).Status != metav1.ConditionTrue || result.RequeueAfter != naasQualificationResync {
		t.Fatalf("fresh record: %d jobs, %+v, %s", len(w.jobs(t)), w.condition(t), result.RequeueAfter)
	}
	// Inside the renewal window: a Job starts and the old record still holds.
	write(w.now.Add(-28*day), key)
	w.reconcile(t)
	if len(w.jobs(t)) != 1 || w.condition(t).Status != metav1.ConditionTrue || !strings.Contains(w.condition(t).Message, "renewal is running") {
		t.Fatalf("renewal: %d jobs, %+v", len(w.jobs(t)), w.condition(t))
	}
	// The key changes mid-run: the stale Job is removed and one for the new
	// key starts; the old record no longer qualifies the cluster.
	parent := w.parent(t)
	parent.Status.PlatformComponents[0].InstalledVersion = "0.16.0"
	if err := w.management.Status().Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	w.now = w.now.Add(time.Minute)
	w.reconcile(t)
	jobs := w.jobs(t)
	if len(jobs) != 1 || jobs[0].Labels[naas.LabelQualificationKey] != w.key(t).Hash() || w.condition(t).Reason != reasonQualificationRunning {
		t.Fatalf("key change: %v, %+v", jobs, w.condition(t))
	}
}

func TestNaaSQualificationWithoutAQualifierImage(t *testing.T) {
	w := newQualificationWorld(t, "")
	w.reconcile(t)
	if len(w.jobs(t)) != 0 || w.condition(t).Reason != reasonQualifierNotSet {
		t.Fatalf("no image: %d jobs, %+v", len(w.jobs(t)), w.condition(t))
	}
}

// Clusters that are not NaaS are left alone.
func TestNaaSQualificationIgnoresOtherClusters(t *testing.T) {
	w := newQualificationWorld(t, "registry.example/cv:1")
	parent := w.parent(t)
	parent.Spec.NaaS = nil
	if err := w.management.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	if result := w.reconcile(t); result.RequeueAfter != 0 || len(w.jobs(t)) != 0 || w.condition(t) != nil {
		t.Fatalf("a non-NaaS cluster was qualified: %+v", w.condition(t))
	}
}

func TestNaaSQualificationVerdict(t *testing.T) {
	key := naas.ClusterQualificationKey{ClusterUID: "u", BootstrapRevision: "r", KubernetesMinor: "1.35", CapsuleVersion: "0.15.2"}
	now := time.Now()
	passed := &naas.ClusterQualification{Key: key, IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Hour).Add(naas.MaxQualificationValidity), Checks: allPassed()}
	_, _, _, after := naasQualificationVerdict(passed, nil, key, now, false, true)
	if after != naasQualificationResync {
		t.Fatalf("settled resync = %s", after)
	}
	nearRenewal := &naas.ClusterQualification{Key: key, IssuedAt: now.Add(-27*24*time.Hour + time.Minute), Checks: allPassed()}
	nearRenewal.ExpiresAt = nearRenewal.IssuedAt.Add(naas.MaxQualificationValidity)
	if _, _, _, after := naasQualificationVerdict(nearRenewal, nil, key, now, false, true); after > 2*time.Minute {
		t.Fatalf("the next look is after the renewal start: %s", after)
	}
	if status, reason, _, _ := naasQualificationVerdict(nil, naas.ErrNoQualification, key, now, false, true); status != metav1.ConditionFalse || reason != reasonQualificationRequired {
		t.Fatalf("no record = %s %s", status, reason)
	}
}
