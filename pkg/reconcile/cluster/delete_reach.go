package cluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// apiProbeTimeout bounds the one call that asks a workload cluster whether
// its API answers.
const apiProbeTimeout = 15 * time.Second

// releaseHint is what a stage tells about letting the Helm Releases go.
const releaseHint = "`cluster delete --force` lets them go at once"

// apiDownSince is when each deleting cluster's workload API was first seen
// not answering in the platform stages, to let its Helm Releases go after
// the teardown timeout. A restart forgets it: the timeout then counts again.
var apiDownSince sync.Map

// forgetDeleteClocks drops what the passes of a deletion remember about a
// cluster (the node group stage's errors, the API's downtime).
func forgetDeleteClocks(id string) {
	nodeStageErrors.Delete(id)
	apiDownSince.Delete(id)
}

// skipCause says why a deletion does not use the workload cluster's API,
// without calling it: the deletion is forced, there is no kubeconfig Secret,
// or the EKS cluster is in a status that does not answer (not ACTIVE or
// UPDATING: CREATING, DELETING, FAILED, or none). Empty when the API is to
// be tried; a try that fails is retried until the teardown timeout by the
// stages that use it.
func (s *Reconciler) skipCause(ctx context.Context, row *model.Cluster, records []model.ManagedResource) (string, error) {
	if row.DeleteForce {
		return "the deletion is forced", nil
	}
	if storedStatus(row).GetKubeconfigSecret() == "" {
		return "the cluster has no kubeconfig Secret", nil
	}
	status, err := s.eksStatus(ctx, records)
	if err != nil {
		return "", err
	}
	switch {
	case status == "":
		return "the cluster has no EKS cluster", nil
	case !eksServes(status):
		return fmt.Sprintf("its EKS cluster is %s", status), nil
	}
	return "", nil
}

// downCause is skipCause, and then asks the API (a short call): why the
// workload cluster cannot be used now, empty when it answers. Without a way
// to reach workload clusters (Deps.Teardowns) it is not asked.
func (s *Reconciler) downCause(ctx context.Context, row *model.Cluster, records []model.ManagedResource) (cause string, structural bool, err error) {
	if cause, err = s.skipCause(ctx, row, records); err != nil || cause != "" {
		return cause, true, err
	}
	if s.deps.Teardowns == nil {
		return "", false, nil
	}
	if why := s.probeAPI(ctx, row); why != "" {
		return "the cluster's API cannot be reached: " + why, false, nil
	}
	return "", false, nil
}

// probeAPI asks the workload cluster for something small: a kind it may not
// serve (no answer is an answer). It returns why the API did not answer;
// empty when it did.
func (s *Reconciler) probeAPI(ctx context.Context, row *model.Cluster) string {
	ctx, cancel := context.WithTimeout(ctx, apiProbeTimeout)
	defer cancel()
	td, err := s.deps.Teardowns.Open(ctx, kube.TenantNamespace(row.TenantID), storedStatus(row).GetKubeconfigSecret())
	if err != nil {
		return err.Error()
	}
	if _, err = td.List(ctx, reconcile.KindNodePool); err != nil {
		return err.Error()
	}
	return ""
}

// releaseDue reports whether the Helm Releases of a cluster whose workload
// API is down are to be let go now, and how long they are waited for
// otherwise: at once when the cause does not change with time (structural:
// forced, no kubeconfig, an EKS cluster that does not answer) or when the
// teardown stage already waited the teardown timeout and gave up; else
// after the teardown timeout from the first pass that found the API down.
func (s *Reconciler) releaseDue(row *model.Cluster, structural bool) (bool, time.Duration) {
	if structural {
		return true, 0
	}
	if result := storedStatus(row).GetTeardown().GetResult(); result == cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_TIMED_OUT ||
		result == cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_SKIPPED {
		return true, 0
	}
	at := now()
	first, _ := apiDownSince.LoadOrStore(row.ID, at)
	left := s.teardownTimeout() - at.Sub(first.(time.Time))
	return left <= 0, left
}

// releaseIfDown is the deletion's step for a workload cluster whose API
// cannot be used (downCause): its Helm Releases are let go without an
// uninstall (forceRelease), at once when the cause is structural or the
// teardown already gave up, else after the teardown timeout from the first
// pass that found the API down, so a cluster that is only restarting is
// uninstalled as usual. A Warning event tells when they are let go. It
// returns what the stage's message adds: the cause, and what happens to the
// Releases. Empty when the API answers.
func (s *Reconciler) releaseIfDown(ctx context.Context, row *model.Cluster, records []model.ManagedResource) (string, error) {
	cause, structural, err := s.downCause(ctx, row, records)
	if err != nil {
		return "", err
	}
	if cause == "" {
		apiDownSince.Delete(row.ID)
		return "", nil
	}
	due, left := s.releaseDue(row, structural)
	if !due {
		return fmt.Sprintf("%s; its Helm Releases are let go without an uninstall in %s (%s)",
			cause, left.Round(time.Second), releaseHint), nil
	}
	n, err := s.letGoReleases(ctx, row)
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("%s; its Helm Releases are let go without an uninstall", cause)
	if !row.DeleteForce {
		msg += " (`cluster delete --force` skips the wait)"
	}
	if n > 0 {
		reconcile.AddNote(ctx, reconcile.Warning(reconcile.ReasonReleasesLetGo, fmt.Sprintf(
			"%d Helm Releases are let go without an uninstall: %s; what they installed may be left in the cluster", n, cause)))
	}
	return msg, nil
}
