package catalog

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yindia/cluster-vending/pkg/component"
)

// SwitchedOff are the notes `--check` prints for each definition the set
// disables (spec.disabled) that other definitions depend on: the
// dependents the disable switches off with it (component.Off, the cascade
// decided on 2026-11-20), so the review sees the whole effect. Not
// problems: the set stays valid. Sorted.
func SwitchedOff(set Set) []string {
	defs, _ := definitionsOf(set.Sort())
	off := component.Off(defs)
	var notes []string
	for _, d := range defs {
		if !d.Disabled {
			continue
		}
		dependents := dependentsOf(d.Name, defs)
		dependents = slices.DeleteFunc(dependents, func(name string) bool { _, isOff := off[name]; return !isOff })
		if len(dependents) == 0 {
			continue
		}
		notes = append(notes, fmt.Sprintf("ComponentDefinition/%s is disabled: it switches off %s, which depend on it",
			d.Name, strings.Join(dependents, ", ")))
	}
	slices.Sort(notes)
	return notes
}

// dependentsOf are the definitions that require name, directly or through
// others, or are members of it (a bundle), sorted.
func dependentsOf(name string, defs []component.Definition) []string {
	seen := map[string]bool{name: true}
	queue := []string{name}
	var out []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, d := range defs {
			if seen[d.Name] {
				continue
			}
			bundle := false
			for _, b := range defs {
				if b.Name == cur && slices.Contains(b.Includes, d.Name) {
					bundle = true
				}
			}
			if slices.Contains(d.Requires, cur) || bundle {
				seen[d.Name] = true
				out = append(out, d.Name)
				queue = append(queue, d.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}
