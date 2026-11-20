package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/clustersummary"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/policy"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/reconcile/resultsink"
	"github.com/yindia/cluster-vending/pkg/results"
)

// PolicyReconciler observes workload policy data, writing only management
// Cluster status. Workload errors do not affect provisioning state.
type PolicyReconciler struct {
	deps Deps
	// sink writes the full snapshot to the cluster's results bucket; full:
	// the collection is not bounded by the Cluster object's caps (the S3
	// object takes what the cluster has), and the Cluster object keeps a
	// bounded copy.
	sink *resultsink.Sink
	full bool
	// history decides when a cluster's violations get a history object (at
	// most one an hour). Violation events never get one: they are kept for the
	// last hour only (PROP-02 5.4).
	history *historyGate
	collect func(context.Context, []byte, time.Time) (*cloudv1.ClusterPolicySnapshot, error)
}

func NewPolicyReconciler(d Deps) *PolicyReconciler {
	full := d.ResultsS3 != nil
	return &PolicyReconciler{deps: d, full: full, history: &historyGate{}, sink: resultsink.New(d.Deps, d.logger()),
		collect: func(ctx context.Context, k []byte, now time.Time) (*cloudv1.ClusterPolicySnapshot, error) {
			r, e := policy.NewReader(k)
			if e != nil {
				return nil, e
			}
			r.Full = full
			return r.Collect(ctx, now), nil
		}}
}
func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("cluster-policy").For(&cloudv1alpha1.Cluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).Complete(r)
}
func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	leave := r.deps.FreezeBarrier.Enter()
	defer leave()
	obj := &cloudv1alpha1.Cluster{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	id := obj.Labels[cloudstore.LabelID]
	frozen, err := r.deps.Freeze(ctx, id)
	if err != nil {
		return ctrl.Result{}, err
	}
	if id == "" || obj.Labels[cloudstore.LabelTenant] == "" || frozen.Holds() || managed.IsPaused(obj) || !policyScannable(obj) {
		return ctrl.Result{RequeueAfter: notScannableDelay}, nil
	}
	now := time.Now()
	last := policy.Decode([]byte(obj.Status.PolicySnapshot)).Summary.GetCollectedAt()
	if last == nil {
		// The snapshot lives in the results bucket: its summary has the time.
		for _, ref := range obj.Status.Results {
			if ref.Kind == cloudv1alpha1.ResultKindViolations && ref.CollectedAt != nil {
				last = timestamppb.New(ref.CollectedAt.Time)
			}
		}
	}
	if last != nil {
		if remaining := policy.Interval - now.Sub(last.AsTime()); remaining > 0 && remaining <= policy.Interval {
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}
	timeout := policy.Timeout
	if r.full {
		timeout = policy.FullTimeout
	}
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	k, err := reconcile.ReadKubeconfig(call, r.deps.Direct, obj.Namespace, obj.Status.KubeconfigSecret)
	var snapshot *cloudv1.ClusterPolicySnapshot
	if err == nil {
		snapshot, err = r.collect(call, k, now)
	}
	collected := err == nil
	if err != nil {
		snapshot = policy.Stale(policy.Decode([]byte(obj.Status.PolicySnapshot)), now, "Check controller access and kubeconfig.")
	}
	if ctx.Err() != nil {
		return ctrl.Result{}, ctx.Err()
	}
	if snapshot == nil || snapshot.Summary == nil {
		snapshot = policy.Empty("Workload policy collector returned no snapshot.")
	}
	// What the scan collected, whole, goes to the results bucket (only a
	// collection that worked: a stale or empty one keeps what is there);
	// the Cluster object keeps a bounded copy and the summaries.
	staged, notices := r.stage(ctx, obj, snapshot, now, collected)
	snapshot = proto.CloneOf(snapshot)
	for _, n := range notices {
		if len(snapshot.Summary.Notices) < 32 {
			snapshot.Summary.Notices = append(snapshot.Summary.Notices, n)
		}
	}
	raw, err := policy.Encode(snapshot)
	if err != nil {
		return ctrl.Result{}, err
	}
	// The scan can take tens of seconds, so obj.ResourceVersion is stale by now
	// and other status writers (the cluster reconciler) must not make it rerun.
	// Patch only status.policySnapshot; the UID precondition (checked by the API
	// server, and again here against a fresh read) refuses to write onto an
	// object that was deleted and recreated under the same namespace/name.
	fresh := &cloudv1alpha1.Cluster{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if fresh.UID != obj.UID {
		return ctrl.Result{}, apierrors.NewConflict(cloudv1alpha1.GroupVersion.WithResource("clusters").GroupResource(), obj.Name, errors.New("cluster was recreated during the scan"))
	}
	status := map[string]any{"policySnapshot": string(raw)}
	if collected {
		// The counts of the cluster status summary (PROP-02 5.2); a failed
		// collection keeps the ones recorded.
		status["violations"], status["violationEvents"] = clustersummary.ViolationCounts(snapshot, now)
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": obj.UID},
		"status":   status,
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	err = r.deps.Direct.Status().Patch(ctx, fresh, client.RawPatch(types.MergePatchType, patch))
	if err == nil {
		// Once the Cluster object holds its bounded copy: recording a summary
		// drops the lists the bucket now has.
		for _, st := range staged {
			if cerr := st.Commit(ctx); cerr != nil {
				r.deps.logger().WarnContext(ctx, "policy summary not recorded", "tenant_id", obj.Labels[cloudstore.LabelTenant], "cluster_id", id, "error", cerr)
			}
		}
	}
	return ctrl.Result{RequeueAfter: policy.Interval}, client.IgnoreNotFound(err)
}

// policyScannable: collection only reads the workload cluster, so it runs
// in every phase that has a workload cluster to read (a Failed or Updating
// cluster must not hide its violations): its kubeconfig exists and it is
// neither being created nor deleted.
func policyScannable(obj *cloudv1alpha1.Cluster) bool {
	if !obj.DeletionTimestamp.IsZero() || obj.Status.KubeconfigSecret == "" {
		return false
	}
	switch obj.Status.Phase {
	case cloudv1alpha1.ClusterPhaseRunning, cloudv1alpha1.ClusterPhaseUpdating, cloudv1alpha1.ClusterPhaseFailed,
		cloudv1alpha1.ClusterPhaseImported:
		return true
	case cloudv1alpha1.ClusterPhaseProvisioning, cloudv1alpha1.ClusterPhaseDeleting:
		return false
	}
	return false
}

// stage writes the snapshot's violations and its events to the results
// bucket, as two results of kind violations and violation-events. It returns
// them for Commit and the notices for the bounded copy the Cluster object
// keeps (one per result that stayed there).
func (r *PolicyReconciler) stage(ctx context.Context, obj *cloudv1alpha1.Cluster, snapshot *cloudv1.ClusterPolicySnapshot, now time.Time, collected bool) ([]*resultsink.Staged, []string) {
	if !collected || snapshot == nil || snapshot.Summary == nil || snapshot.Summary.CollectedAt == nil {
		return nil, nil
	}
	tenant, id := obj.Labels[cloudstore.LabelTenant], obj.Labels[cloudstore.LabelID]
	var staged []*resultsink.Staged
	var notices []string
	for _, part := range []struct {
		kind    results.Kind
		message *cloudv1.ClusterPolicySnapshot
		total   int
		history bool
	}{
		{results.Violations, &cloudv1.ClusterPolicySnapshot{Summary: snapshot.Summary, Violations: snapshot.Violations}, len(snapshot.Violations), true},
		{results.ViolationEvents, &cloudv1.ClusterPolicySnapshot{Summary: snapshot.Summary, Events: snapshot.Events}, len(snapshot.Events), false},
	} {
		withHistory := part.history && (r.history == nil || r.history.due(id, now))
		opts := policyPutOptions(withHistory)
		st, notice := r.sink.Stage(ctx, tenant, id, part.kind, part.message, now, model.ResultRef{
			Digest: policyDigest(part.message), CollectedAt: snapshot.Summary.CollectedAt.AsTime(), AttemptedAt: now,
			Total: int32(part.total), Incomplete: snapshot.Summary.Incomplete, Truncated: snapshot.Summary.Truncated}, opts...) //nolint:gosec // bounded by the object size
		if st != nil && !st.Unchanged && withHistory && r.history != nil {
			r.history.wrote(id, now)
		}
		if st != nil {
			staged = append(staged, st)
		}
		if notice != "" {
			notices = append(notices, notice)
		}
	}
	return staged, dedupe(notices)
}

// policyPutOptions are the writer's options for a policy result: latest only
// unless its history object is due.
func policyPutOptions(withHistory bool) []results.PutOption {
	if withHistory {
		return nil
	}
	return []results.PutOption{results.WithoutHistory()}
}

// policyDigest is the digest of a policy result without what changes on every
// scan: the collection time and the violations' observation times.
func policyDigest(p *cloudv1.ClusterPolicySnapshot) string {
	c := proto.CloneOf(p)
	c.Summary.CollectedAt = nil
	for _, v := range c.Violations {
		v.ObservedAt = nil
	}
	return resultsink.Digest(c)
}

func dedupe(in []string) []string {
	var out []string
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
