package component

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// ConditionValid is a ComponentDefinition's condition that its values,
// schema and cloud resources are usable (CheckDefinition).
const ConditionValid = "Valid"

// DefinitionOf is a ComponentDefinition as the checks and the merge take
// it. Valid is the object's Valid condition; a definition the controller
// has not judged yet is not valid.
func DefinitionOf(def *cloudv1alpha1.ComponentDefinition) (Definition, error) {
	platform, err := objectOf(def.Spec.PlatformValues)
	if err != nil {
		return Definition{}, fmt.Errorf("%s: platformValues: %w", def.Name, err)
	}
	schema, err := objectOf(def.Spec.ValuesSchema)
	if err != nil {
		return Definition{}, fmt.Errorf("%s: valuesSchema: %w", def.Name, err)
	}
	return Definition{
		Name:                def.Name,
		Valid:               meta.IsStatusConditionTrue(def.Status.Conditions, ConditionValid),
		Disabled:            def.Spec.Disabled,
		Deleting:            !def.DeletionTimestamp.IsZero(),
		Management:          Management(def.Spec.Management),
		Internal:            def.Spec.Internal,
		Includes:            slices.Clone(def.Spec.Includes),
		HasInstall:          def.Spec.Install != nil,
		InstallTimeout:      installTimeoutOf(def),
		Requires:            slices.Clone(def.Spec.Requires),
		Sequence:            sequenceOf(def),
		Provider:            string(def.Spec.Provider),
		Chart:               chartOf(def),
		Namespace:           def.Spec.Namespace,
		ConflictsWithAddons: slices.Clone(def.Spec.ConflictsWithAddons),
		PlatformValues:      platform,
		UserValues:          def.Spec.UserValues,
		ValuesSchema:        schema,
		CloudResources:      CloudResourcesOf(def.Spec.CloudResources),
	}, nil
}

// installTimeoutOf is spec.install.timeout; 0 when unset.
func installTimeoutOf(def *cloudv1alpha1.ComponentDefinition) time.Duration {
	if def.Spec.Install == nil || def.Spec.Install.Timeout == nil {
		return 0
	}
	return def.Spec.Install.Timeout.Duration
}

// objectOf decodes a schemaless field that must hold a JSON object; nil
// when unset.
func objectOf(raw *runtime.RawExtension) (Values, error) {
	if raw == nil || len(raw.Raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw.Raw, &v); err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be an object, got %s", typeName(v))
	}
	return obj, nil
}

// WithPinValues is d as it applies under a release pin: the values the
// release sets for the component on the cluster's Kubernetes minor
// (ComponentPin.values) are merged over the definition's platformValues,
// the pin winning. They are platform values from then on: locked for
// tenants (LockedPathsTouched), last in the merge, and their placeholders
// are filled in with the rest. d is not changed.
func WithPinValues(d Definition, pin Values) Definition {
	out := d
	out.PlatformValues = mergeInto(mergeInto(Values{}, d.PlatformValues), pin)
	return out
}

// PinValues decodes a release pin's values: an object whose placeholders
// are all known. nil when unset.
func PinValues(raw *runtime.RawExtension) (Values, error) {
	v, err := objectOf(raw)
	if err != nil {
		return nil, err
	}
	if err := Validate(v); err != nil {
		return nil, err
	}
	return v, nil
}

// chartOf is "<repository> <chart>" of the definition's main chart; empty
// without one.
func chartOf(def *cloudv1alpha1.ComponentDefinition) string {
	if def.Spec.Install == nil || def.Spec.Install.Helm == nil {
		return ""
	}
	return def.Spec.Install.Helm.Repository + " " + def.Spec.Install.Helm.Chart
}

// DefaultSequence is the sequence of a definition that sets none.
const DefaultSequence int32 = 100

// sequenceOf is the definition's spec.sequence, DefaultSequence when unset.
func sequenceOf(def *cloudv1alpha1.ComponentDefinition) int32 {
	if def.Spec.Sequence == nil {
		return DefaultSequence
	}
	return *def.Spec.Sequence
}
