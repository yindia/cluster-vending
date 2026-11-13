package reconcile_test

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func TestRecentKeepsTheLastEvents(t *testing.T) {
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	earlier := timestamppb.New(at.Add(-time.Hour))
	kept := func(reason, message string, count int32) *cloudv1.Event {
		return &cloudv1.Event{Time: earlier, Type: cloudv1.EventType_EVENT_TYPE_NORMAL, Reason: reason, Message: message, Count: count}
	}
	var full []*cloudv1.Event
	for i := range reconcile.MaxRecentEvents {
		full = append(full, kept("Updating", fmt.Sprintf("generation %d", i), 1))
	}
	tests := []struct {
		name     string
		before   []*cloudv1.Event
		happened []reconcile.Note
		want     []string // "Reason xCount"
	}{
		{name: "nothing kept, nothing happened"},
		{name: "the first event", happened: []reconcile.Note{reconcile.Normal("Provisioning", "creating")},
			want: []string{"Provisioning x1"}},
		{name: "another event is another row", before: []*cloudv1.Event{kept("Provisioning", "creating", 1)},
			happened: []reconcile.Note{reconcile.Normal("Running", "ready")}, want: []string{"Provisioning x1", "Running x1"}},
		{name: "the same event again counts", before: []*cloudv1.Event{kept("Failed", "NodeGroup ng: x", 1)},
			happened: []reconcile.Note{reconcile.Warning("Failed", "NodeGroup ng: x")}, want: []string{"Failed x2"}},
		{name: "the same reason with another message is another row", before: []*cloudv1.Event{kept("Failed", "NodeGroup ng: x", 1)},
			happened: []reconcile.Note{reconcile.Warning("Failed", "Addon a: y")}, want: []string{"Failed x1", "Failed x1"}},
		{name: "nothing happened keeps what was", before: []*cloudv1.Event{kept("Running", "ready", 1)}, want: []string{"Running x1"}},
		{name: "only the last ten stay", before: full, happened: []reconcile.Note{reconcile.Normal("Running", "ready")},
			want: []string{"Updating x1", "Updating x1", "Updating x1", "Updating x1", "Updating x1", "Updating x1", "Updating x1",
				"Updating x1", "Updating x1", "Running x1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reconcile.Recent(tt.before, tt.happened, at)

			var have []string
			for _, e := range got {
				have = append(have, fmt.Sprintf("%s x%d", e.GetReason(), e.GetCount()))
			}
			if strings.Join(have, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("recent = %v, want %v", have, tt.want)
			}
			for _, e := range tt.before {
				if !e.GetTime().AsTime().Equal(earlier.AsTime()) {
					t.Error("the events kept before were changed")
				}
			}
			if n := len(tt.happened); n > 0 && !got[len(got)-1].GetTime().AsTime().Equal(at) {
				t.Errorf("the newest event's time = %v, want %v", got[len(got)-1].GetTime().AsTime(), at)
			}
		})
	}
}

func TestRecentCarriesTheType(t *testing.T) {
	got := reconcile.Recent(nil, []reconcile.Note{reconcile.Warning("Failed", "x"), reconcile.Normal("Running", "y")}, time.Now())

	if got[0].GetType() != cloudv1.EventType_EVENT_TYPE_WARNING || got[1].GetType() != cloudv1.EventType_EVENT_TYPE_NORMAL {
		t.Fatalf("types = %v, %v", got[0].GetType(), got[1].GetType())
	}
}

// A pass's steps leave notes in its context; a context that collects none
// drops them.
func TestNotesOfAPass(t *testing.T) {
	plain := context.Background()
	reconcile.AddNote(plain, reconcile.Normal("Repaired", "x"))
	if got := reconcile.Notes(plain); got != nil {
		t.Fatalf("a plain context kept %v", got)
	}

	ctx := reconcile.WithNotes(plain)
	reconcile.AddNote(ctx, reconcile.Normal("Repaired", "recreated Role r"))
	reconcile.AddNote(reconcile.WithNotes(ctx), reconcile.Warning("Failed", "y")) // the same collector

	got := reconcile.Notes(ctx)
	if len(got) != 2 || got[0].Reason != "Repaired" || got[1].Type != reconcile.EventWarning {
		t.Fatalf("notes = %+v", got)
	}
}

func TestEmitWithoutEventsIsANoOp(t *testing.T) {
	reconcile.Deps{}.Emit(t.Context(), reconcile.Object{Kind: "Cluster", Name: "prod"}, reconcile.EventNormal, "Running", "ready")

	rec := &reconcile.RecordedEvents{}
	reconcile.Deps{Events: rec}.Emit(t.Context(), reconcile.Object{Kind: "Cluster", Name: "prod"}, reconcile.EventWarning, "Failed", "x")
	if got := rec.All(); len(got) != 1 || got[0].Reason != "Failed" || got[0].Object.Name != "prod" {
		t.Fatalf("recorded %+v", got)
	}
}

func TestPassFailure(t *testing.T) {
	failed := func(message string) []*cloudv1.Condition {
		return []*cloudv1.Condition{{Type: "Reconciled", Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, Message: message}}
	}
	tests := []struct {
		name          string
		before, after []*cloudv1.Condition
		want          string
	}{
		{name: "no failure"},
		{name: "a failure appears", after: failed("no network n"), want: "could not apply the change: no network n"},
		{name: "the same failure again", before: failed("no network n"), after: failed("no network n")},
		{name: "another failure", before: failed("no network n"), after: failed("provider missing"), want: "could not apply the change: provider missing"},
		{name: "the failure is gone", before: failed("no network n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reconcile.PassFailure(tt.before, tt.after, "Reconciled")

			if tt.want == "" {
				if len(got) != 0 {
					t.Fatalf("notes = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Type != reconcile.EventWarning || got[0].Reason != reconcile.ReasonFailed || got[0].Message != tt.want {
				t.Fatalf("notes = %+v, want one warning %q", got, tt.want)
			}
		})
	}
}

func TestPauseNotes(t *testing.T) {
	paused := []*cloudv1.Condition{{Type: reconcile.ConditionPaused, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
		Message: "3 of 40 resources are paused"}}
	tests := []struct {
		name          string
		before, after []*cloudv1.Condition
		want          string
	}{
		{name: "never paused"},
		{name: "a pause starts", after: paused, want: reconcile.ReasonPaused},
		{name: "the pause goes on", before: paused, after: paused},
		{name: "the pause ends", before: paused, want: reconcile.ReasonResumed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reconcile.PauseNotes(tt.before, tt.after)

			if tt.want == "" {
				if len(got) != 0 {
					t.Fatalf("notes = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Reason != tt.want || got[0].Type != reconcile.EventNormal || got[0].Message == "" {
				t.Fatalf("notes = %+v, want one %s", got, tt.want)
			}
		})
	}
}

// The packages that decide what happens know events only through
// reconcile.Events: none of them imports an event recorder.
func TestReconcilePackagesImportNoRecorder(t *testing.T) {
	banned := []string{"k8s.io/client-go/tools/record", "k8s.io/client-go/tools/events", "sigs.k8s.io/controller-runtime/pkg/recorder"}
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			for _, b := range banned {
				if strings.Trim(imp.Path.Value, `"`) == b {
					t.Errorf("%s imports %s", path, b)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
