package cluster

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// nodeStagePass runs the node groups' stage once over row, records the
// status it computed on row (as a pass's record does) and returns whether
// the stage is over and the notes it told.
func nodeStagePass(t *testing.T, s *Reconciler, row *model.Cluster) (bool, []reconcile.Note) {
	t.Helper()
	ctx := withTeardownHolder(reconcile.WithNotes(t.Context()))
	done := s.deleteNodeGroups(ctx, row, &clusterRemoval{}, true)
	st := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_DELETING}
	withDeleteClocksStatus(ctx, st, storedStatus(row))
	raw, err := Encode(st)
	require.NoError(t, err)
	row.Status = raw
	return done, reconcile.Notes(ctx)
}

// The node groups' stage has one clock: the teardown timeout counts from
// the stage's start, not from each of its parts (pools, claims, classes),
// so a stage whose pools took most of the timeout times out soon after
// its claims start. The timeout is recorded as the stage's finish, so its
// warning is told once, however many passes run before the next stage.
func TestNodeStageTimesOutOnceFromItsStart(t *testing.T) {
	clock := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	f := &fakeTeardowns{stuck: true, objs: []reconcile.WorkloadObject{
		obj(reconcile.KindNodePool, "", "gpu", false),
		obj(reconcile.KindNodeClaim, "", "gpu-abc", false),
		obj(reconcile.KindEC2NodeClass, "", "gpu", false),
	}}
	s := New(reconcile.Deps{Teardowns: f})
	row := &model.Cluster{Base: model.Base{ID: "c-node-clock"}, TenantID: "acme"}
	t.Cleanup(func() { forgetDeleteClocks(row.ID) })

	done, _ := nodeStagePass(t, s, row) // deletes the pool
	require.False(t, done)
	start := storedStatus(row).GetNodeTeardown().GetStartTime()
	require.NotNil(t, start, "the stage's start is in the status")
	require.Equal(t, clock, start.AsTime())

	// The pool takes 14 of the 15 minutes; then the claims are deleted.
	clock = clock.Add(14 * time.Minute)
	f.stuck = false
	done, _ = nodeStagePass(t, s, row)
	require.False(t, done)
	f.stuck = true
	require.Equal(t, start.AsTime(), storedStatus(row).GetNodeTeardown().GetStartTime().AsTime(), "one start for the stage")

	clock = clock.Add(2 * time.Minute) // 16 minutes into the stage, 2 into the claims
	done, notes := nodeStagePass(t, s, row)
	require.True(t, done, "the timeout counts from the stage's start")
	require.Equal(t, reconcile.ReasonTeardownTimedOut, reasons(notes))
	nt := storedStatus(row).GetNodeTeardown()
	require.Equal(t, cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_TIMED_OUT, nt.GetResult())
	require.NotNil(t, nt.GetFinishTime())

	for range 3 { // later passes, before the next stage shows it began
		clock = clock.Add(time.Minute)
		done, notes = nodeStagePass(t, s, row)
		require.True(t, done)
		require.Empty(t, notes, "the timeout is told once")
	}
}
