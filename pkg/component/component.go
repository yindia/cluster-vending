// Package component decides, without any client or store, whether a tenant
// may enable, change or disable a component on a cluster, and what values
// the component is installed with (docs/design/component-usage.md,
// sections 4.4, 5 and 8):
//
//   - Merge builds the effective values: chart defaults, then the
//     definition's userValues file, then the tenant's overrides, then the
//     definition's platformValues; maps merge key by key, lists replace,
//     an explicit null removes a default.
//   - LockedPathsTouched names every tenant value that touches a locked
//     path (a leaf path of platformValues): at it, below it, or replacing
//     a parent of it. PruneUnchanged keeps only the tenant's edits of the
//     userValues file.
//   - ValidateSchema checks the tenant's values against the definition's
//     values schema (a JSON Schema subset, see schema.go); CheckDefinition
//     checks a definition's own values for its Valid condition.
//   - CheckEnable runs the twelve request checks of section 4.4 and
//     returns one error listing every problem; CheckDisable and
//     CheckAddons are the reverse checks of section 8.
//
// It is pure: no Kubernetes client, no store, no contract or kind types.
// The callers (the component handlers, the controller adopting objects
// created with kubectl) read the definition, the cluster, its components
// and the release offering and pass plain values. Errors are faults
// (pkg/fault) with the kind the API answers with.
package component

import (
	"encoding/json"
	"fmt"

	"github.com/yindia/cluster-vending/pkg/fault"
)

// MaxValuesSize is the largest tenant values object, serialized as JSON.
const MaxValuesSize = 64 * 1024

// Values is a values object: what JSON decodes an object into.
type Values = map[string]any

// Definition is a catalog entry (ComponentDefinition), as far as the
// checks and the merge need it.
type Definition struct {
	Name string
	// Valid is the definition's Valid condition: an invalid entry counts as
	// not in the catalog.
	Valid bool
	// Management says who controls the component; empty is Tenant.
	Management Management
	// Internal: only installed as a member of a bundle.
	Internal bool
	// Includes are the members of a bundle; empty for a component with a
	// chart of its own.
	Includes []string
	// HasInstall: the definition has an install of its own (spec.install).
	HasInstall bool
	// Requires are components that must be enabled first.
	Requires []string
	// ConflictsWithAddons are EKS add-on names the cluster must not have
	// while this component is enabled.
	ConflictsWithAddons []string
	// PlatformValues are the platform team's values for every tenant,
	// locked: every leaf path in them wins, and a tenant who touches one
	// is refused.
	PlatformValues Values
	// UserValues is the values file tenants pull, edit and send back:
	// YAML with the platform team's comments (ParseUserValues).
	UserValues string
	// ValuesSchema is what a tenant may override, written by hand; nil: no
	// overrides.
	ValuesSchema Values
	// CloudResources the platform creates in the tenant's account
	// (CloudResourcesOf).
	CloudResources CloudResources
}

// ParseValues reads a tenant's values from JSON: an object of at most
// MaxValuesSize bytes. Empty input is no values.
func ParseValues(raw []byte) (Values, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > MaxValuesSize {
		return nil, fault.Newf(fault.Invalid, "values are %d bytes; at most %d are accepted", len(raw), MaxValuesSize)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fault.Newf(fault.Invalid, "values are not valid JSON: %v", err)
	}
	if v == nil {
		return nil, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fault.Newf(fault.Invalid, "values must be an object, got %s", typeName(v))
	}
	return obj, nil
}

// valuesSize is the serialized size of values.
func valuesSize(v Values) (int, error) {
	if len(v) == 0 {
		return 0, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, fmt.Errorf("values cannot be serialized: %w", err)
	}
	return len(raw), nil
}
