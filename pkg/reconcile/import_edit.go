package reconcile

import (
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

// ConditionSpecEditIgnored is the condition of an imported network or
// cluster whose spec was changed on its object, behind the API's back
// (kubectl edit), and put back: True from then until the takeover.
const ConditionSpecEditIgnored = "SpecEditIgnored"

// DiscoveredKey is the key, in the JSON object an import keeps with its
// row (a cluster's adoption, a network's input), of what the import
// discovered as the row reads it back: Discovered.
const DiscoveredKey = "importDiscovered"

// Discovered is an imported row as its import left it: the spec derived
// from what exists, the display name and the labels. While the row is
// imported and not taken over these are read-only: the API refuses to
// change them, and a change made on the object is put back from here, so
// the plan a takeover applies is the plan that was reviewed.
type Discovered struct {
	Spec        json.RawMessage   `json:"spec"`
	DisplayName string            `json:"displayName,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	// EditIgnoredAt is when a change made on the object was last put back
	// (RFC 3339); empty when none was.
	EditIgnoredAt string `json:"editIgnoredAt,omitempty"`
}

// DiscoveredOf is the Discovered kept in stored (a JSON object), or false
// when it keeps none: a row the platform created, or one imported before
// it was kept.
func DiscoveredOf(stored model.JSON) (Discovered, bool, error) {
	var d Discovered
	if len(stored) == 0 {
		return d, false, nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(stored, &fields); err != nil {
		return d, false, fmt.Errorf("stored import data: %w", err)
	}
	raw, ok := fields[DiscoveredKey]
	if !ok {
		return d, false, nil
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, false, fmt.Errorf("stored discovered spec: %w", err)
	}
	return d, true, nil
}

// WithDiscovered is stored (a JSON object, or empty) with d kept in it.
func WithDiscovered(stored model.JSON, d Discovered) (model.JSON, error) {
	fields := map[string]json.RawMessage{}
	if len(stored) > 0 {
		if err := json.Unmarshal(stored, &fields); err != nil {
			return nil, fmt.Errorf("stored import data: %w", err)
		}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	fields[DiscoveredKey] = raw
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return model.JSON(out), nil
}

// Snapshot is r as a Discovered.
func Snapshot(r *model.Resource) Discovered {
	return Discovered{Spec: json.RawMessage(r.Spec), DisplayName: r.DisplayName, Labels: maps.Clone(map[string]string(r.Labels))}
}

// Restore writes d back onto r, and reports whether anything differed:
// the display name, the labels, or the spec (sameSpec compares two stored
// specs by what they say, not by their bytes).
func (d Discovered) Restore(r *model.Resource, sameSpec func(a, b model.JSON) bool) bool {
	same := r.DisplayName == d.DisplayName && maps.Equal(map[string]string(r.Labels), d.Labels) && sameSpec(r.Spec, model.JSON(d.Spec))
	if same {
		return false
	}
	r.Spec, r.DisplayName, r.Labels = model.JSON(d.Spec), d.DisplayName, model.Labels(maps.Clone(d.Labels))
	return true
}

// Ignored is d with the time an edit was put back.
func (d Discovered) Ignored(at time.Time) Discovered {
	d.EditIgnoredAt = at.UTC().Format(time.RFC3339)
	return d
}
