package cluster

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// fakeTeardowns is one workload cluster whose controllers remove an
// object a pass after its deletion was asked for (gone).
type fakeTeardowns struct {
	objs    []reconcile.WorkloadObject
	deleted []string
	err     error // of opening the cluster
	stuck   bool  // deleted objects never go away
	// onDelete is told of each deletion asked for.
	onDelete func(reconcile.WorkloadObject)
}

func (f *fakeTeardowns) Open(context.Context, string, string) (reconcile.Teardown, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f, nil
}

func (f *fakeTeardowns) List(_ context.Context, kinds ...string) ([]reconcile.WorkloadObject, error) {
	var kept, out []reconcile.WorkloadObject
	for _, o := range f.objs {
		if o.Deleting && !f.stuck {
			continue // its controller removed it since the last pass
		}
		kept = append(kept, o)
		if slices.Contains(kinds, o.Kind) {
			out = append(out, o)
		}
	}
	f.objs = kept
	return out, nil
}

func (f *fakeTeardowns) Delete(_ context.Context, o reconcile.WorkloadObject) error {
	if f.onDelete != nil {
		f.onDelete(o)
	}
	f.deleted = append(f.deleted, o.Ref())
	for i := range f.objs {
		if f.objs[i].Ref() == o.Ref() {
			f.objs[i].Deleting, f.objs[i].DeletedAt = true, now()
		}
	}
	return nil
}

// pass runs one teardown pass over prev and returns whether the deletion
// may go on, the state it recorded and the events it told.
func pass(t *testing.T, s *Reconciler, row *model.Cluster, prev *cloudv1.ClusterTeardown, reachable bool) (bool, *cloudv1.ClusterTeardown, []reconcile.Note) {
	t.Helper()
	ctx := withTeardownHolder(reconcile.WithNotes(t.Context()))
	done := s.runTeardown(ctx, row, prev, reachable)
	st := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_DELETING}
	withTeardownStatus(ctx, st, &cloudv1.ClusterStatus{})
	return done, st.GetTeardown(), reconcile.Notes(ctx)
}

func reasons(notes []reconcile.Note) string {
	var out []string
	for _, n := range notes {
		out = append(out, n.Reason)
	}
	return strings.Join(out, ",")
}

// The load balancer stage deletes Ingresses and Services only, then the
// deletion goes on, told once at the start and once at the end; Karpenter's
// objects are not its business.
func TestTeardownDeletesTheLoadBalancers(t *testing.T) {
	start := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return start }
	t.Cleanup(func() { now = time.Now })
	f := &fakeTeardowns{objs: []reconcile.WorkloadObject{
		obj(reconcile.KindService, "istio-system", "istio-ingressgateway", false),
		obj(reconcile.KindNodePool, "", "default", false),
		obj(reconcile.KindEC2NodeClass, "", "default", false),
	}}
	s := New(reconcile.Deps{Teardowns: f})
	row := &model.Cluster{Base: model.Base{ID: "c1"}, TenantID: "acme"}

	done, td, notes := pass(t, s, row, nil, true)
	if done || reasons(notes) != reconcile.ReasonTeardownStarted || len(f.deleted) != 1 {
		t.Fatalf("pass 1: done %v, notes %s, deleted %v", done, reasons(notes), f.deleted)
	}
	if !strings.Contains(td.GetMessage(), "1 load balancer (Service istio-system/istio-ingressgateway)") {
		t.Errorf("pass 1 message = %q", td.GetMessage())
	}
	done, td, notes = pass(t, s, row, td, true)
	if !done || td.GetResult() != cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_FINISHED ||
		reasons(notes) != reconcile.ReasonTeardownFinished {
		t.Fatalf("pass 2: done %v, %v, notes %s", done, td, reasons(notes))
	}
	if want := "Service istio-system/istio-ingressgateway"; strings.Join(f.deleted, "|") != want {
		t.Errorf("deleted %v, want only %s", f.deleted, want)
	}
	if !td.GetStartTime().AsTime().Equal(start) {
		t.Errorf("start = %v, want the first pass's", td.GetStartTime().AsTime())
	}
}

// While it runs, the deleting cluster's message is the stage and what it
// waits for.
func TestTeardownIsTheDeletingMessage(t *testing.T) {
	f := &fakeTeardowns{objs: []reconcile.WorkloadObject{obj(reconcile.KindService, "web", "shop", false)}}
	s := New(reconcile.Deps{Teardowns: f})
	ctx := withDeleteStage(withTeardownHolder(reconcile.WithNotes(t.Context())))
	s.runTeardown(ctx, &model.Cluster{TenantID: "acme"}, nil, true)
	st := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_DELETING, Message: "deleting; 30 resources left"}

	withTeardownStatus(ctx, st, &cloudv1.ClusterStatus{})
	withDeleteStageStatus(ctx, st)

	if want := "deleting; stage 1 of 7, load balancers: deleting 1 load balancer (Service web/shop)"; st.GetMessage() != want {
		t.Fatalf("message = %q, want %q", st.GetMessage(), want)
	}
}

// After the timeout the deletion goes on, with a warning naming what is
// left; a forced deletion or an unreachable API skips it with a warning.
func TestTeardownEnds(t *testing.T) {
	start := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	started := &cloudv1.ClusterTeardown{StartTime: timestamppb.New(start)}
	tests := []struct {
		name       string
		row        *model.Cluster
		reachable  bool
		f          *fakeTeardowns
		at         time.Time
		wantDone   bool
		wantResult cloudv1.ClusterTeardownResult
		wantReason string
		wantText   string
	}{
		{name: "timed out with a load balancer left", row: &model.Cluster{}, reachable: true,
			f:  &fakeTeardowns{stuck: true, objs: []reconcile.WorkloadObject{obj(reconcile.KindService, "web", "shop", true)}},
			at: start.Add(16 * time.Minute), wantDone: true,
			wantResult: cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_TIMED_OUT,
			wantReason: reconcile.ReasonTeardownTimedOut, wantText: "Service web/shop"},
		{name: "before the timeout it waits", row: &model.Cluster{}, reachable: true,
			f:  &fakeTeardowns{stuck: true, objs: []reconcile.WorkloadObject{obj(reconcile.KindService, "web", "shop", true)}},
			at: start.Add(5 * time.Minute), wantDone: false},
		{name: "an API it cannot reach is retried until the timeout", row: &model.Cluster{}, reachable: true,
			f: &fakeTeardowns{err: errors.New("connection refused")}, at: start.Add(5 * time.Minute), wantDone: false},
		{name: "then the deletion goes on", row: &model.Cluster{}, reachable: true,
			f: &fakeTeardowns{err: errors.New("connection refused")}, at: start.Add(15 * time.Minute), wantDone: true,
			wantResult: cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_TIMED_OUT,
			wantReason: reconcile.ReasonTeardownTimedOut, wantText: "connection refused"},
		{name: "forced", row: &model.Cluster{DeleteForce: true}, reachable: false, f: &fakeTeardowns{},
			at: start, wantDone: true, wantResult: cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_SKIPPED,
			wantReason: reconcile.ReasonTeardownSkipped, wantText: "the deletion is forced"},
		{name: "API not reachable", row: &model.Cluster{}, reachable: false, f: &fakeTeardowns{},
			at: start, wantDone: true, wantResult: cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_SKIPPED,
			wantReason: reconcile.ReasonTeardownSkipped, wantText: "network delete then waits"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now = func() time.Time { return tt.at }
			t.Cleanup(func() { now = time.Now })
			s := New(reconcile.Deps{Teardowns: tt.f})

			done, td, notes := pass(t, s, tt.row, started, tt.reachable)

			if done != tt.wantDone || td.GetResult() != tt.wantResult {
				t.Fatalf("done %v result %v, want %v %v (%q)", done, td.GetResult(), tt.wantDone, tt.wantResult, td.GetMessage())
			}
			if tt.wantReason != "" && (reasons(notes) != tt.wantReason || !strings.Contains(notes[0].Message, tt.wantText) ||
				notes[0].Type != reconcile.EventWarning) {
				t.Errorf("notes = %+v, want a %s warning with %q", notes, tt.wantReason, tt.wantText)
			}
		})
	}
}

// A finished teardown is not run again; without a way to reach workload
// clusters there is none.
func TestTeardownRunsOnce(t *testing.T) {
	f := &fakeTeardowns{objs: []reconcile.WorkloadObject{obj(reconcile.KindService, "web", "shop", false)}}
	s := New(reconcile.Deps{Teardowns: f})
	raw, err := Encode(&cloudv1.ClusterStatus{Teardown: &cloudv1.ClusterTeardown{FinishTime: timestamppb.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Cluster{Resource: model.Resource{Status: raw}}

	done, err := s.teardown(t.Context(), row, nil)

	if !done || err != nil || len(f.deleted) != 0 {
		t.Fatalf("done %v err %v deleted %v", done, err, f.deleted)
	}
	if done, err := New(reconcile.Deps{}).teardown(t.Context(), &model.Cluster{}, nil); !done || err != nil {
		t.Fatalf("without Teardowns: done %v err %v", done, err)
	}
}
