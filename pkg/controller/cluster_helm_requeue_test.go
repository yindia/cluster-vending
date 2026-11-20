package controller

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// A cluster with a Helm release in backoff is looked at again when the
// backoff ends, not at the next resync of a RUNNING cluster.
func TestClusterIsLookedAtWhenAHelmBackoffEnds(t *testing.T) {
	row := func(retry time.Time) *model.Cluster {
		raw, err := reconcilecluster.Encode(&cloudv1.ClusterStatus{PlatformComponents: []*cloudv1.PlatformComponent{{
			Name: "kyverno", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Attempts: 2, NextRetryAt: timestamppb.New(retry)}}})
		if err != nil {
			t.Fatal(err)
		}
		return &model.Cluster{Resource: model.Resource{State: reconcilecluster.StateRunning, Generation: 2,
			ObservedGeneration: 2, AppliedGeneration: 2, Status: raw}}
	}

	soon := nextCluster(row(time.Now().Add(90*time.Second)), nil).RequeueAfter
	overdue := nextCluster(row(time.Now().Add(-time.Minute)), nil).RequeueAfter
	far := nextCluster(row(time.Now().Add(6*time.Hour)), nil).RequeueAfter

	if soon <= 60*time.Second || soon > 90*time.Second {
		t.Errorf("a backoff ending in 90s: requeue after %s", soon)
	}
	if overdue != retryFloor {
		t.Errorf("an overdue backoff: requeue after %s, want %s", overdue, retryFloor)
	}
	if far != resyncDelay {
		t.Errorf("a far backoff must leave the resync alone: %s, want %s", far, resyncDelay)
	}
}
