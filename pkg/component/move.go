package component

import "reflect"

// MoveGroup is a component as a release move upgrades it: its name and
// the companions (charts pinned under their own names) that move with it.
type MoveGroup struct {
	Name       string
	Companions []string
}

// Settled reports whether the component name runs at pin: its chart
// version is the pin's, the provider applied it, and it is Ready.
type Settled func(name string, pin Pin) bool

// Move is one pass of moving a cluster's components from one release's
// pins to another's.
type Move struct {
	// Pins are what to install this pass.
	Pins map[string]Pin
	// Component is the one being moved now; "" when done.
	Component string
	// Step is its place among the Steps components that change (1-based);
	// when done, Step is Steps.
	Step, Steps int
	// Done: every changed component runs at the target's pins; Pins are
	// the target's.
	Done bool
}

// PlanMove moves a cluster from the pins from to the pins to, one
// component at a time in order (install order, dependencies first): a
// component takes its target pin only when every changed component before
// it is settled at its own (settled); the ones after keep their current
// pin, and one only in the target is not installed before its turn. A
// component only in from is kept until the move is done, then left out
// (removed last). A component whose pins do not change is not a step.
func PlanMove(order []MoveGroup, from, to map[string]Pin, settled Settled) Move {
	var changed []MoveGroup
	for _, g := range order {
		if _, inTarget := to[g.Name]; inTarget && groupChanged(g, from, to) {
			changed = append(changed, g)
		}
	}
	m := Move{Steps: len(changed)}
	for i, g := range changed {
		if settled != nil && settled(g.Name, to[g.Name]) {
			continue
		}
		m.Component, m.Step = g.Name, i+1
		m.Pins = mixed(from, to, changed[:i+1])
		return m
	}
	m.Done, m.Step, m.Pins = true, m.Steps, copyPins(to)
	return m
}

// mixed are from's pins with the groups moved taking to's.
func mixed(from, to map[string]Pin, moved []MoveGroup) map[string]Pin {
	out := copyPins(from)
	for _, g := range moved {
		for _, name := range g.names() {
			if p, ok := to[name]; ok {
				out[name] = p
			} else {
				delete(out, name)
			}
		}
	}
	return out
}

func groupChanged(g MoveGroup, from, to map[string]Pin) bool {
	for _, name := range g.names() {
		a, inFrom := from[name]
		b, inTo := to[name]
		if inFrom != inTo || a.Version != b.Version || !reflect.DeepEqual(a.Values, b.Values) {
			return true
		}
	}
	return false
}

func (g MoveGroup) names() []string { return append([]string{g.Name}, g.Companions...) }

func copyPins(in map[string]Pin) map[string]Pin {
	out := make(map[string]Pin, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// MoveOrder are the entries as move groups, in their order.
func MoveOrder(entries []PlatformEntry) []MoveGroup {
	out := make([]MoveGroup, 0, len(entries))
	for _, e := range entries {
		g := MoveGroup{Name: e.Name}
		for _, c := range e.Install.Companions {
			g.Companions = append(g.Companions, c.Name)
		}
		out = append(out, g)
	}
	return out
}
