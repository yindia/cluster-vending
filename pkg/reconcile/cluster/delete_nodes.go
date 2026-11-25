package cluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	awsevents "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchevents/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awssqs "github.com/upbound/provider-aws/v2/apis/namespaced/sqs/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// clusterRemoval is the cluster's own objects in one pass of a deletion:
// the records and their views as the pass started.
type clusterRemoval struct {
	records []model.ManagedResource
	views   map[string]*cloudv1.ManagedResource
	prefix  string
}

func (s *Reconciler) loadClusterRemoval(ctx context.Context, row *model.Cluster, records []model.ManagedResource) (*clusterRemoval, error) {
	c := &clusterRemoval{records: records, prefix: crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name)}
	err := c.observe(ctx, s)
	return c, err
}

func (c *clusterRemoval) observe(ctx context.Context, s *Reconciler) error {
	views, err := s.deps.Observe(ctx, c.records)
	if err != nil {
		return err
	}
	c.views = make(map[string]*cloudv1.ManagedResource, len(views))
	for _, v := range views {
		c.views[viewKey(v.GetKind(), v.GetName())] = v
	}
	return nil
}

func (c *clusterRemoval) gone(r model.ManagedResource) bool {
	v, ok := c.views[viewKey(r.Kind, r.Name)]
	return !ok || v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING
}

// stage are the records (never Usages) of one stage of the deletion.
func (c *clusterRemoval) stage(stage crossplaneaws.ClusterDeleteStage) []model.ManagedResource {
	var out []model.ManagedResource
	for _, r := range c.records {
		if r.Kind != managed.UsageKind && crossplaneaws.ClusterObjectDeleteStage(c.prefix, r.Kind, r.Name) == stage {
			out = append(out, r)
		}
	}
	return out
}

func (c *clusterRemoval) left(rs []model.ManagedResource) []model.ManagedResource {
	var out []model.ManagedResource
	for _, r := range rs {
		if !c.gone(r) {
			out = append(out, r)
		}
	}
	return out
}

// past reports whether Karpenter's stage began: an object of it went
// (GoneAt is stored only by the deletion's stages after this one), or is
// being deleted while none of its objects that no Usage holds is left. It is
// the node groups' stage's memory (nothing else is stored for it): it is not
// run again once what comes after it started, however the workload cluster
// looks then. Only Karpenter's objects count: a later stage's (a node group
// an update replaced) may be going since before the cluster's deletion was
// asked for. So may one of Karpenter's that an update replaced (a renamed
// rule): the deletion's own removal deletes every Karpenter object at once,
// and those no Usage holds (targets, queue policy, access entry) go into
// deletion at once, so one of them still there means the deletion did not
// start it.
func (c *clusterRemoval) past() bool {
	deleting, unheldLeft := false, false
	for _, r := range c.stage(crossplaneaws.DeleteStageKarpenter) {
		if r.GoneAt != nil {
			return true
		}
		switch c.views[viewKey(r.Kind, r.Name)].GetState() {
		case cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING:
			deleting = true
		case cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING:
		default:
			unheldLeft = unheldLeft || !usageHeld(r.Kind)
		}
	}
	return deleting && !unheldLeft
}

// usageHeld reports whether objects of the kind may be the object of a
// Karpenter Usage (crossplaneaws karpenterUsagePairs), whose deletion the
// Usage webhook refuses until its users are gone: the node role and its
// policy attachments, the queue, the rules. Targets, the queue policy and
// the access entry are only ever users, deleted when asked.
func usageHeld(kind string) bool {
	switch kind {
	case awssqs.QueuePolicy_Kind, awsevents.Target_Kind, awseks.AccessEntry_Kind:
		return false
	}
	return true
}

// removeStage deletes the cluster's objects of one stage (Karpenter's, the
// add-ons') and reports whether they are all gone. What is gone is marked,
// and Usages whose user went are released.
func (s *Reconciler) removeStage(ctx context.Context, c *clusterRemoval, stage crossplaneaws.ClusterDeleteStage, stageNo int, what string) (bool, error) {
	objects := c.left(c.stage(stage))
	if len(objects) == 0 {
		return true, nil
	}
	if stage == crossplaneaws.DeleteStageKarpenter {
		if err := s.releaseNodeRoleUsages(ctx, c); err != nil {
			return false, err
		}
	}
	if err := s.deps.DeleteRecorded(ctx, objects); err != nil {
		return false, deleteError(err)
	}
	if err := c.observe(ctx, s); err != nil {
		return false, err
	}
	views := make([]*cloudv1.ManagedResource, 0, len(c.records))
	for _, r := range c.records {
		views = append(views, c.views[viewKey(r.Kind, r.Name)])
	}
	if err := s.deps.MarkGone(ctx, c.records, views); err != nil {
		return false, err
	}
	if err := s.deps.ReleaseOrphanUsages(ctx, c.records, views); err != nil {
		return false, err
	}
	left := c.left(objects)
	if len(left) == 0 {
		return true, nil
	}
	waits := fmt.Sprintf("%s %s", left[0].Kind, left[0].Name)
	if len(left) > 1 {
		waits += fmt.Sprintf(" and %d more", len(left)-1)
	}
	setDeleteStage(ctx, stageNo, what+"; waiting for "+waits)
	return false, nil
}

// releaseNodeRoleUsages deletes the Usages that keep Karpenter's node role
// and its policy attachments until the EKS cluster is gone
// (crossplaneaws karpenterUsagePairs): the deletion removes Karpenter before
// the cluster, when its nodes are gone and nothing uses the role.
func (s *Reconciler) releaseNodeRoleUsages(ctx context.Context, c *clusterRemoval) error {
	for _, r := range c.records {
		if r.Kind != managed.UsageKind || c.gone(r) {
			continue
		}
		u, err := s.deps.ReadRecorded(ctx, r)
		if err != nil {
			return fmt.Errorf("read Usage %s: %w", r.Name, err)
		}
		if u == nil {
			continue
		}
		ofKind, _, _ := unstructured.NestedString(u.Object, "spec", "of", "kind")
		ofName, _, _ := unstructured.NestedString(u.Object, "spec", "of", "resourceRef", "name")
		byKind, _, _ := unstructured.NestedString(u.Object, "spec", "by", "kind")
		if byKind != "Cluster" || !crossplaneaws.IsKarpenterNodeIAM(c.prefix, ofKind, ofName) {
			continue
		}
		if err := s.deps.Kube.Delete(ctx, u); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("release Usage %s: %w", r.Name, err)
		}
	}
	return nil
}

// nodeStageErrors is when each deleting cluster's workload API was first
// not reachable in the node groups' stage, to end the wait after the
// teardown timeout. A restart forgets it: the timeout then counts again.
var nodeStageErrors sync.Map

// deleteNodeGroups is stage 4: in the workload cluster, Karpenter's
// node pools (the users', and an old platform pool) are deleted, then what is
// left of its NodeClaims is waited for, then its EC2NodeClasses are deleted (Karpenter
// removes their instance profiles). Each waits until the one before is gone.
// Nothing is deleted before the platform components above Karpenter are
// gone, so nothing is scheduled onto the nodes being removed. Reads are
// bounded (one page per kind, the pass deadline) and a wait ends after the
// teardown timeout from the request to delete, with a warning naming what is
// left (the deletion goes on). It reports whether the stage is over; a
// cluster whose API cannot be reached is skipped (the teardown said so).
// The stage never fails the deletion: what goes wrong is its status message.
func (s *Reconciler) deleteNodeGroups(ctx context.Context, row *model.Cluster, c *clusterRemoval, reachable bool) bool {
	if s.deps.Teardowns == nil || !reachable || c.past() {
		nodeStageErrors.Delete(row.ID)
		return true
	}
	at := now()
	step, err := s.stepNodeGroups(ctx, row)
	if err != nil {
		first, _ := nodeStageErrors.LoadOrStore(row.ID, at)
		if at.Sub(first.(time.Time)) >= s.teardownTimeout() {
			nodeStageErrors.Delete(row.ID)
			return s.nodeStageTimedOut(ctx, row, "the cluster's API could not be reached: "+err.Error(), nil)
		}
		s.logger().WarnContext(ctx, "node groups teardown", "cluster_id", row.ID, "error", err)
		setDeleteStage(ctx, stageNodeGroups, "cannot reach the cluster's API: "+err.Error()+"; trying again")
		return false
	}
	nodeStageErrors.Delete(row.ID)
	if step.finished() {
		return true
	}
	if !step.since.IsZero() && at.Sub(step.since) >= s.teardownTimeout() {
		return s.nodeStageTimedOut(ctx, row, step.message, step.waiting)
	}
	setDeleteStage(ctx, stageNodeGroups, step.message)
	return false
}

func (s *Reconciler) stepNodeGroups(ctx context.Context, row *model.Cluster) (teardownStep, error) {
	ctx, cancel := context.WithTimeout(ctx, nodeGroupTotalTimeout)
	defer cancel()
	td, err := s.deps.Teardowns.Open(ctx, kube.TenantNamespace(row.TenantID), storedStatus(row).GetKubeconfigSecret())
	if err != nil {
		return teardownStep{}, err
	}
	objs, err := td.List(ctx, stageKinds(nodeGroupStages)...)
	if err != nil {
		return teardownStep{}, err
	}
	step := planTeardown(nodeGroupStages, objs)
	for _, o := range step.delete {
		if err := td.Delete(ctx, o); err != nil {
			return teardownStep{}, fmt.Errorf("delete %s: %w", o.Ref(), err)
		}
		s.logger().InfoContext(ctx, "node groups teardown: deleting", "cluster_id", row.ID, "object", o.Ref())
	}
	return step, nil
}

// nodeStageTimedOut ends the stage after the teardown timeout: the deletion
// goes on, and a warning says what is still there.
func (s *Reconciler) nodeStageTimedOut(ctx context.Context, row *model.Cluster, what string, left []reconcile.WorkloadObject) bool {
	msg := fmt.Sprintf("node groups: timed out after %s (%s); going on: Karpenter's EC2 instances, launch templates and instance profiles "+
		"may be left in the account; network delete then waits until they are deleted", s.teardownTimeout(), what)
	reconcile.AddNote(ctx, reconcile.Warning(reconcile.ReasonTeardownTimedOut, msg))
	s.logger().WarnContext(ctx, "node groups teardown timed out", "cluster_id", row.ID, "left", refs(left))
	return true
}
