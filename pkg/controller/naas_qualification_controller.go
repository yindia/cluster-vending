package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/naas"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
)

// Pacing of the NaaS qualification controller.
const (
	// naasQualificationRunningResync: a qualifier Job is in flight.
	naasQualificationRunningResync = 30 * time.Second
	// naasQualificationResync: a settled cluster is looked at again; the
	// key's inputs (versions, OIDC) change without a generation bump.
	naasQualificationResync = 10 * time.Minute
	// naasQualificationPendingResync: the cluster cannot be read yet.
	naasQualificationPendingResync = 2 * time.Minute
)

// Reasons of the NaaSQualified condition.
const (
	reasonQualificationPassed   = "QualificationPassed"
	reasonQualificationFailed   = "QualificationFailed"
	reasonQualificationRunning  = "QualificationRunning"
	reasonQualificationRequired = "QualificationRequired"
	reasonQualifierNotSet       = "QualifierNotConfigured"
	reasonQualificationPending  = "QualificationPending"
)

// NaaSQualificationReconciler qualifies each NaaS cluster for sub-tenant
// access (PROP-05 6.1): it starts the naas-qualifier Job when the cluster has
// no current record for its key (first enable, a bootstrap, Capsule, Istio,
// Kubernetes or OIDC change, or the renewal before 30 days), records the
// Job's result as the cluster's record, and sets the Cluster condition
// NaaSQualified. The old record keeps access open while a renewal runs.
type NaaSQualificationReconciler struct {
	deps Deps
	now  func() time.Time
}

func NewNaaSQualificationReconciler(d Deps) *NaaSQualificationReconciler {
	return &NaaSQualificationReconciler{deps: d, now: time.Now}
}

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=clusters/status,verbs=get;update;patch

func (r *NaaSQualificationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.deps.FreezeBarrier != nil {
		leave := r.deps.FreezeBarrier.Enter()
		defer leave()
	}
	obj := &cloud.Cluster{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if obj.Spec.NaaS == nil || !*obj.Spec.NaaS || !obj.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	d := r.deps.Deps
	d.Kube, d.KubeReader = r.deps.Direct, r.deps.Direct
	frozen, err := d.SyncFreeze(ctx, platform.PauseScope{TenantID: obj.Labels[cloudstore.LabelTenant], ClusterID: obj.Labels[cloudstore.LabelID], NetworkID: obj.Labels[cloudstore.LabelNetworkID]})
	if err != nil {
		return ctrl.Result{RequeueAfter: naasQualificationPendingResync}, nil
	}
	if frozen.Holds() || managed.IsPaused(obj) {
		// No workload write while frozen; the record and condition stand.
		return untilFreezeEnds(frozen, ctrl.Result{RequeueAfter: naasQualificationResync}, r.now()), nil
	}
	if r.deps.SubTenantWorkloads == nil || obj.Status.KubeconfigSecret == "" {
		return r.report(ctx, obj, metav1.ConditionFalse, reasonQualificationPending, "Waiting for the cluster's kubeconfig and a workload client", naasQualificationPendingResync)
	}
	workload, err := r.deps.SubTenantWorkloads.Client(ctx, obj.Namespace, obj.Status.KubeconfigSecret)
	if err != nil || workload == nil {
		return r.report(ctx, obj, metav1.ConditionFalse, reasonQualificationPending, "Cannot connect to the workload cluster", naasQualificationPendingResync)
	}
	bootstrap, err := naas.VerifyTenantBootstrap(ctx, workload)
	if err != nil {
		return r.report(ctx, obj, metav1.ConditionFalse, reasonQualificationPending, "Waiting for the Capsule bootstrap: "+err.Error(), naasQualificationPendingResync)
	}
	spec := convert.ClusterSpecFromAPI(obj.Spec, obj.Labels[cloudstore.LabelNetworkID], nil)
	key, err := naas.ClusterQualificationKeyFor(obj, bootstrap.Revision(), spec)
	if err != nil {
		return r.report(ctx, obj, metav1.ConditionFalse, reasonQualificationPending, err.Error(), naasQualificationPendingResync)
	}
	return r.qualify(ctx, obj, workload, key)
}

// qualify records finished runs, starts one when needed and reports.
func (r *NaaSQualificationReconciler) qualify(ctx context.Context, obj *cloud.Cluster, workload client.Client, key naas.ClusterQualificationKey) (ctrl.Result, error) {
	now := r.now()
	running, err := r.collectJobs(ctx, workload, key, now)
	if err != nil {
		return r.report(ctx, obj, metav1.ConditionFalse, reasonQualificationPending, "Cannot read or record the qualifier Jobs: "+err.Error(), naasQualificationPendingResync)
	}
	record, readErr := naas.ReadClusterQualification(ctx, workload)
	if readErr != nil {
		record = nil // missing or malformed: a run replaces it
	}
	started := false
	if !running && record.NeedsRun(key, now) && r.deps.NaaS.QualifierImage != "" {
		if err := r.startJob(ctx, workload, key, now); err != nil {
			return r.report(ctx, obj, metav1.ConditionFalse, reasonQualificationPending, "Cannot start the qualifier Job: "+err.Error(), naasQualificationPendingResync)
		}
		running, started = true, true
	}
	status, reason, message, after := naasQualificationVerdict(record, readErr, key, now, running, r.deps.NaaS.QualifierImage != "")
	result, err := r.report(ctx, obj, status, reason, message, after)
	if started && err == nil {
		r.deps.Emit(ctx, platform.Object{Kind: platform.KindCluster, Namespace: obj.Namespace, Name: obj.Name}, platform.EventNormal, reasonQualificationRunning, "Started the naas-qualifier Job")
	}
	return result, err
}

// naasQualificationVerdict is the NaaSQualified condition and the next look.
func naasQualificationVerdict(record *naas.ClusterQualification, readErr error, key naas.ClusterQualificationKey, now time.Time, running, configured bool) (metav1.ConditionStatus, string, string, time.Duration) {
	var expires time.Time
	err := readErr
	if record != nil {
		expires, err = record.Current(key, now)
	}
	after := naasQualificationResync
	if running {
		after = naasQualificationRunningResync
	}
	var failed *naas.QualificationFailedError
	switch {
	case err == nil:
		message := fmt.Sprintf("Passed all %d checks; valid until %s", len(naas.RequiredQualificationChecks()), expires.UTC().Format(time.RFC3339))
		if running {
			message += "; a renewal is running and the current record stays valid meanwhile"
		}
		renew := expires.Add(-naas.QualificationRenewBefore).Sub(now)
		return metav1.ConditionTrue, reasonQualificationPassed, message, max(min(after, renew), soon)
	case errors.As(err, &failed):
		message := "Failed checks: " + strings.Join(failed.Checks, ", ")
		if reasons := failureReasons(record, failed.Checks); reasons != "" {
			message += " (" + reasons + ")"
		}
		if running {
			message += "; a new run is in progress"
		}
		return metav1.ConditionFalse, reasonQualificationFailed, message, after
	case running:
		return metav1.ConditionFalse, reasonQualificationRunning, "The naas-qualifier Job is running: " + err.Error(), after
	case !configured:
		return metav1.ConditionFalse, reasonQualifierNotSet, "naas.qualifierImage is not set, so no qualifier Job runs; an operator may write the record by hand: " + err.Error(), after
	default:
		return metav1.ConditionFalse, reasonQualificationRequired, err.Error(), after
	}
}

func failureReasons(record *naas.ClusterQualification, checks []string) string {
	var out []string
	for _, check := range checks {
		if why := record.Failures[check]; why != "" {
			out = append(out, check+": "+why)
		}
		if len(out) == 3 {
			break
		}
	}
	return strings.Join(out, "; ")
}

// collectJobs records each finished qualifier Job of key and removes it,
// removes Jobs of other keys (their result could not be recorded), and
// reports whether a Job of key is still running.
func (r *NaaSQualificationReconciler) collectJobs(ctx context.Context, workload client.Client, key naas.ClusterQualificationKey, now time.Time) (bool, error) {
	jobs := &batchv1.JobList{}
	if err := workload.List(ctx, jobs, client.InNamespace(naas.QualificationNamespace), client.MatchingLabels{naas.LabelQualifier: "true"}); err != nil {
		return false, err
	}
	running := false
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !job.DeletionTimestamp.IsZero() {
			continue
		}
		finished, failure := jobFinished(job)
		switch {
		case job.Labels[naas.LabelQualificationKey] != key.Hash():
			if err := deleteJob(ctx, workload, job); err != nil {
				return false, err
			}
		case !finished:
			running = true
		default:
			if err := r.recordJob(ctx, workload, key, job, failure, now); err != nil {
				return false, err
			}
			if err := deleteJob(ctx, workload, job); err != nil {
				return false, err
			}
		}
	}
	return running, nil
}

// jobFinished: complete, or failed with why.
func jobFinished(job *batchv1.Job) (bool, string) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete, batchv1.JobSuccessCriteriaMet:
			return true, ""
		case batchv1.JobFailed, batchv1.JobFailureTarget:
			return true, strings.TrimSpace("the qualifier Job failed: " + c.Reason + " " + c.Message)
		}
	}
	return false, ""
}

// recordJob writes the cluster's record from a finished Job: its result when
// it is this Job's for this key, else a failed run (which closes access).
func (r *NaaSQualificationReconciler) recordJob(ctx context.Context, workload client.Client, key naas.ClusterQualificationKey, job *batchv1.Job, failure string, now time.Time) error {
	ref := naas.QualifierJobRef{Name: job.Name, UID: string(job.UID)}
	result, err := naas.ReadQualifierResult(ctx, workload)
	var record *naas.ClusterQualification
	switch {
	case err == nil && result != nil && job.UID != "" && result.JobUID == string(job.UID) && result.Key == key:
		record = result.Record(ref, now)
	default:
		if failure == "" {
			failure = "the qualifier Job ended without a result of its own"
		}
		record = naas.FailedRunRecord(key, ref, failure, now)
	}
	desired, err := naas.ClusterQualificationConfigMap(record)
	if err != nil {
		return err
	}
	if err := applyConfigMap(ctx, workload, desired); err != nil {
		return fmt.Errorf("write the cluster qualification record: %w", err)
	}
	if result != nil {
		stale := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: naas.QualificationNamespace, Name: naas.QualifierResultName}}
		if err := workload.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func deleteJob(ctx context.Context, workload client.Client, job *batchv1.Job) error {
	err := workload.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	return client.IgnoreNotFound(err)
}

func applyConfigMap(ctx context.Context, workload client.Client, desired *corev1.ConfigMap) error {
	current := &corev1.ConfigMap{}
	err := workload.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if apierrors.IsNotFound(err) {
		return workload.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	next := current.DeepCopy()
	next.Data, next.Labels = desired.Data, desired.Labels
	return workload.Update(ctx, next)
}

// startJob applies the qualifier's ServiceAccount and RBAC and creates the Job.
func (r *NaaSQualificationReconciler) startJob(ctx context.Context, workload client.Client, key naas.ClusterQualificationKey, now time.Time) error {
	objects, err := naas.RenderQualifier(naas.QualifierOptions{Image: r.deps.NaaS.QualifierImage, ProbeImage: r.deps.NaaS.QualifierProbeImage(), Key: key, At: now})
	if err != nil {
		return err
	}
	// A result left by an earlier run must not be taken for this one's.
	stale := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: naas.QualificationNamespace, Name: naas.QualifierResultName}}
	if err := workload.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	for _, obj := range objects {
		if _, job := obj.(*batchv1.Job); job {
			if err := workload.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create %s: %w", obj.GetName(), err)
			}
			continue
		}
		if err := applyObject(ctx, workload, obj); err != nil {
			return fmt.Errorf("apply %T %s: %w", obj, obj.GetName(), err)
		}
	}
	return nil
}

// applyObject creates obj, or replaces what exists with it.
func applyObject(ctx context.Context, workload client.Client, obj client.Object) error {
	current, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not an object", obj)
	}
	err := workload.Get(ctx, client.ObjectKeyFromObject(obj), current)
	if apierrors.IsNotFound(err) {
		return workload.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(current.GetResourceVersion())
	return workload.Update(ctx, obj)
}

// report sets NaaSQualified on the Cluster when it changed, and looks again
// after.
func (r *NaaSQualificationReconciler) report(ctx context.Context, obj *cloud.Cluster, status metav1.ConditionStatus, reason, message string, after time.Duration) (ctrl.Result, error) {
	fresh := &cloud.Cluster{}
	if err := r.deps.Direct.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if fresh.UID != obj.UID {
		return ctrl.Result{}, nil
	}
	previous := meta.FindStatusCondition(fresh.Status.Conditions, cloud.ConditionNaaSQualified)
	condition := metav1.Condition{Type: cloud.ConditionNaaSQualified, Status: status, Reason: reason, Message: message, ObservedGeneration: fresh.Generation}
	if !meta.SetStatusCondition(&fresh.Status.Conditions, condition) {
		return ctrl.Result{RequeueAfter: after}, nil
	}
	if err := r.deps.Direct.Status().Update(ctx, fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if previous == nil || previous.Status != status || previous.Reason != reason {
		eventType := platform.EventNormal
		if status != metav1.ConditionTrue && reason == reasonQualificationFailed {
			eventType = platform.EventWarning
		}
		r.deps.Emit(ctx, platform.Object{Kind: platform.KindCluster, Namespace: obj.Namespace, Name: obj.Name}, eventType, reason, message)
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// SetupWithManager watches Clusters: a generation, label or deletion change
// starts a pass; the key's status inputs are caught by the resync.
func (r *NaaSQualificationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&cloud.Cluster{}, forObject()).Named("naasqualification").WithOptions(objectOptions(r.deps)).Complete(r)
}
