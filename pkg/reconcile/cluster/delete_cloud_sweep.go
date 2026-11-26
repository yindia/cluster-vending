package cluster

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	accountreconcile "github.com/yindia/cluster-vending/pkg/reconcile/account"
)

// cloudSweepPassTimeout bounds the AWS calls of one pass of the sweep;
// what is left is the next pass's.
const cloudSweepPassTimeout = time.Minute

// cloudSweepShown is how many IDs a message names; the status's waiting
// list holds cloudSweepKept.
const (
	cloudSweepShown = 5
	cloudSweepKept  = 50
)

// Events of the cloud sweep (docs/architecture.md#deletion):
//
//	Cluster  CloudSweepFinished  Normal   nothing the cluster's controllers made is left in the account
//	Cluster  CloudSweepTimedOut  Warning  the sweep gave up after the teardown timeout; the message names what is left
//	Cluster  CloudSweepSkipped   Normal   the platform does not delete the cluster's EKS cluster (imported)
const (
	ReasonCloudSweepFinished = "CloudSweepFinished"
	ReasonCloudSweepTimedOut = "CloudSweepTimedOut"
	ReasonCloudSweepSkipped  = "CloudSweepSkipped"
)

// sweepCloud is the cloud side of the deletion, the first step of stage 5
// (before Karpenter's node role and instance profiles are deleted, and so
// before the EKS cluster and the network): with the tenant role, the
// account is searched by the cluster's tags for what its controllers made
// and the workload cluster's own teardown did not delete (the cluster was
// not reachable, the deletion is forced, a stage timed out), and it is
// deleted: Karpenter's instances (terminated, and waited for) and launch
// templates, then the AWS Load Balancer Controller's load balancers, target
// groups and security groups (awscloud.SweepCluster). It runs for every
// deletion of a cluster whose EKS cluster the platform deletes, and reports
// whether the deletion may go on. A sweep that does not finish within the
// teardown timeout (from its own start, kept in the status) ends with a
// Warning naming what is left by ID: the deletion never waits on it for
// ever, and never says nothing.
func (s *Reconciler) sweepCloud(ctx context.Context, row *model.Cluster, records []model.ManagedResource) bool {
	prev := storedStatus(row).GetCloudSweep()
	if prev.GetFinishTime() != nil || s.deps.CloudSweep == nil {
		return true
	}
	at := now()
	t := &cloudv1.ClusterTeardown{StartTime: prev.GetStartTime()}
	if t.StartTime == nil {
		t.StartTime = timestamppb.New(at)
	}
	defer setCloudSweep(ctx, t)
	deletes, err := s.eksDeletedInCloud(ctx, row, records)
	if err != nil {
		s.logger().WarnContext(ctx, "cloud sweep: the EKS cluster cannot be read", "cluster_id", row.ID, "error", err)
		deletes = !row.Imported
	}
	if !deletes {
		t.FinishTime, t.Result = timestamppb.New(at), cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_SKIPPED
		t.Message = "skipped: the platform does not delete this cluster's EKS cluster (imported), so nothing of its controllers is swept"
		reconcile.AddNote(ctx, reconcile.Normal(ReasonCloudSweepSkipped, t.Message))
		return true
	}
	result, why := s.sweepPass(ctx, row)
	if why == "" && result.Done() {
		t.FinishTime, t.Result = timestamppb.New(at), cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_FINISHED
		t.Message = "nothing the cluster's controllers made is left in the account"
		reconcile.AddNote(ctx, reconcile.Normal(ReasonCloudSweepFinished, t.Message))
		return true
	}
	left := result.Left()
	t.Waiting = keep(left, cloudSweepKept)
	what := describeLeft(left)
	if why != "" {
		what = strings.TrimPrefix(what+"; "+why, "; ")
	}
	if at.Sub(t.StartTime.AsTime()) >= s.teardownTimeout() {
		t.FinishTime, t.Result = timestamppb.New(at), cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_TIMED_OUT
		t.Message = fmt.Sprintf("timed out after %s (%s); going on: what is listed may be left in the account "+
			"(and the network's deletion then waits until it is deleted)", s.teardownTimeout(), what)
		reconcile.AddNote(ctx, reconcile.Warning(ReasonCloudSweepTimedOut, t.Message))
		s.logger().WarnContext(ctx, "cloud sweep timed out", "cluster_id", row.ID, "left", left)
		return true
	}
	t.Message = "sweeping the account: " + what
	setDeleteStage(ctx, stageKarpenter, t.Message)
	return false
}

// sweepPass is one bounded pass of the sweep in the cluster's account and
// region. why says what went wrong (empty when nothing did): the pass is
// tried again.
func (s *Reconciler) sweepPass(ctx context.Context, row *model.Cluster) (awscloud.SweepResult, string) {
	log := s.logger().With("cluster_id", row.ID)
	account, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, row.TenantID, row.CloudAccountID)
	if err != nil {
		log.WarnContext(ctx, "cloud sweep: account unreadable", "error", err)
		return awscloud.SweepResult{}, "the cluster's account cannot be read: " + err.Error()
	}
	access, err := s.deps.AWSAccess(ctx, account)
	if err != nil {
		log.WarnContext(ctx, "cloud sweep: account unreachable", "error", err)
		return awscloud.SweepResult{}, "the account cannot be reached: " + err.Error()
	}
	api, err := s.deps.CloudSweep(access, row.Region)
	if err != nil {
		log.WarnContext(ctx, "cloud sweep: account unreachable", "error", err)
		return awscloud.SweepResult{}, "the account cannot be reached: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, cloudSweepPassTimeout)
	defer cancel()
	result, err := awscloud.SweepCluster(ctx, api, row.Name)
	if len(result.Deleted) > 0 {
		log.InfoContext(ctx, "cloud sweep: deleting", "resources", result.Deleted)
	}
	if err != nil {
		log.WarnContext(ctx, "cloud sweep", "error", err)
		why := "AWS: " + err.Error() + "; trying again"
		if hint := accountreconcile.TemplateHint(account, "sweeping the cluster's instances and load balancers",
			accountreconcile.SinceCloudSweep); hint != "" {
			why += " (" + hint + ")"
		}
		return result, why
	}
	return result, ""
}

// eksDeletedInCloud reports whether the cluster's deletion deletes its EKS
// cluster in the cloud: its EKS Cluster object is managed with Delete, or
// is the platform's protected kind the deletion switches to full
// management, and is not retained. An imported cluster that is only
// observed (or retained) keeps its EKS cluster, and its nodes and load
// balancers are not the platform's to delete. With no EKS Cluster object
// left, a cluster the platform created is swept, an imported one is not.
func (s *Reconciler) eksDeletedInCloud(ctx context.Context, row *model.Cluster, records []model.ManagedResource) (bool, error) {
	for _, r := range records {
		if r.Kind != awseks.Cluster_Kind || !strings.HasPrefix(r.APIVersion, "eks.") {
			continue
		}
		c := &awseks.Cluster{}
		if err := s.getIgnoreMissing(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, c); err != nil {
			return false, fmt.Errorf("read EKS cluster %s: %w", r.Name, err)
		}
		if c.Name == "" {
			break // gone
		}
		if c.GetAnnotations()[managed.RetainAnnotation] == "true" {
			return false, nil
		}
		policies := make([]string, 0, len(c.Spec.ManagementPolicies))
		for _, p := range c.Spec.ManagementPolicies {
			policies = append(policies, string(p))
		}
		return len(policies) == 0 || slices.Contains(policies, "*") || slices.Contains(policies, "Delete") ||
			slices.Contains(policies, "Create"), nil
	}
	return !row.Imported, nil
}

// describeLeft names what is left: how many, and the first IDs.
func describeLeft(left []string) string {
	if len(left) == 0 {
		return ""
	}
	shown := keep(left, cloudSweepShown)
	msg := fmt.Sprintf("waiting for %d left: %s", len(left), strings.Join(shown, ", "))
	if len(left) > len(shown) {
		msg += fmt.Sprintf(" and %d more", len(left)-len(shown))
	}
	return msg
}

func keep(ids []string, n int) []string {
	if len(ids) > n {
		return slices.Clone(ids[:n])
	}
	return slices.Clone(ids)
}
