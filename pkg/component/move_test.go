package component_test

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/component"
)

func pin(v string) component.Pin { return component.Pin{Version: v} }

// settledAt says a component is settled when its name maps to the
// version it should be at.
func settledAt(at map[string]string) component.Settled {
	return func(name string, p component.Pin) bool { return at[name] == p.Version }
}

func TestPlanMove(t *testing.T) {
	order := []component.MoveGroup{{Name: "a"}, {Name: "b", Companions: []string{"b-extra"}}, {Name: "c"}, {Name: "d"}}
	from := map[string]component.Pin{"a": pin("1"), "b": pin("1"), "b-extra": pin("1"), "c": pin("1"), "d": pin("1"), "gone": pin("1")}
	to := map[string]component.Pin{"a": pin("1"), "b": pin("2"), "b-extra": pin("2"), "c": pin("2"), "d": pin("1"), "new": pin("1")}
	order = append(order, component.MoveGroup{Name: "new"}, component.MoveGroup{Name: "gone"})

	tests := []struct {
		name          string
		settled       map[string]string
		wantComponent string
		wantStep      int
		wantDone      bool
		want          map[string]string // name -> version planned ("" = not planned)
	}{
		{name: "first changed component starts; later ones wait at the old pin",
			wantComponent: "b", wantStep: 1,
			want: map[string]string{"a": "1", "b": "2", "b-extra": "2", "c": "1", "d": "1", "gone": "1", "new": ""}},
		{name: "the next starts only once the one before is settled",
			settled: map[string]string{"b": "2"}, wantComponent: "c", wantStep: 2,
			want: map[string]string{"b": "2", "c": "2", "new": "", "gone": "1"}},
		{name: "a component only in the target is installed at its turn",
			settled: map[string]string{"b": "2", "c": "2"}, wantComponent: "new", wantStep: 3,
			want: map[string]string{"new": "1", "gone": "1"}},
		{name: "done: the target's pins; one only in the current release is removed last",
			settled: map[string]string{"b": "2", "c": "2", "new": "1"}, wantDone: true, wantStep: 3,
			want: map[string]string{"a": "1", "b": "2", "c": "2", "new": "1", "gone": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := component.PlanMove(order, from, to, settledAt(tt.settled))

			if m.Component != tt.wantComponent || m.Step != tt.wantStep || m.Done != tt.wantDone || m.Steps != 3 {
				t.Fatalf("move = %s %d/%d done=%v, want %s %d/3 done=%v", m.Component, m.Step, m.Steps, m.Done,
					tt.wantComponent, tt.wantStep, tt.wantDone)
			}
			for name, v := range tt.want {
				got, ok := m.Pins[name]
				if v == "" {
					if ok {
						t.Errorf("%s planned at %s, want not planned", name, got.Version)
					}
					continue
				}
				if !ok || got.Version != v {
					t.Errorf("%s planned at %q (%v), want %s", name, got.Version, ok, v)
				}
			}
		})
	}
}

// A values-only change is a change too; a pin without one is not.
func TestPlanMoveValuesChange(t *testing.T) {
	order := []component.MoveGroup{{Name: "a"}}
	from := map[string]component.Pin{"a": {Version: "1", Values: component.Values{"x": 1}}}
	to := map[string]component.Pin{"a": {Version: "1", Values: component.Values{"x": 2}}}

	m := component.PlanMove(order, from, to, func(string, component.Pin) bool { return false })

	if m.Steps != 1 || m.Component != "a" || m.Pins["a"].Values["x"] != 2 {
		t.Fatalf("move = %+v", m)
	}
	if same := component.PlanMove(order, from, from, nil); !same.Done || same.Steps != 0 {
		t.Fatalf("no change: %+v", same)
	}
}
