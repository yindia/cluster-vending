// Package crdtest checks objects against the embedded
// CustomResourceDefinitions (config/crd) offline, as the API server does on
// create with strict field validation: the schema, unknown fields and the
// CEL rules. For tests: it pulls in the API server's validation packages.
package crdtest

import (
	"context"
	"fmt"

	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubejson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/config/crd"
)

// Validator checks objects of one kind.
type Validator struct {
	schema validation.SchemaValidator
	cel    *cel.Validator
	s      *structuralschema.Structural
}

// New is the validator of a kind of the cloud.cluster-vending.io group
// ("ComponentDefinition").
func New(kind string) (*Validator, error) {
	definitions, err := crd.All()
	if err != nil {
		return nil, err
	}
	var def *unstructured.Unstructured
	for _, d := range definitions {
		if k, _, _ := unstructured.NestedString(d.Object, "spec", "names", "kind"); k == kind {
			def = d
		}
	}
	if def == nil {
		return nil, fmt.Errorf("no CustomResourceDefinition of kind %s", kind)
	}
	crdv1 := &apiextensionsv1.CustomResourceDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(def.Object, crdv1); err != nil {
		return nil, err
	}
	internal := &apiextensionsinternal.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crdv1.Spec.Versions[0].Schema.OpenAPIV3Schema, internal, nil); err != nil {
		return nil, err
	}
	schema, _, err := validation.NewSchemaValidator(internal)
	if err != nil {
		return nil, err
	}
	structural, err := structuralschema.NewStructural(internal)
	if err != nil {
		return nil, err
	}
	return &Validator{schema: schema, s: structural, cel: cel.NewValidator(structural, true, celconfig.PerCallLimit)}, nil
}

// Validate returns what the API server would refuse in doc (YAML or JSON):
// unknown fields, schema violations and failed CEL rules.
func (v *Validator) Validate(ctx context.Context, doc []byte) (field.ErrorList, error) {
	obj := map[string]any{}
	if err := decode(doc, &obj); err != nil {
		return nil, err
	}
	var errs field.ErrorList
	for _, path := range pruning.PruneWithOptions(obj, v.s, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true}) {
		errs = append(errs, field.Forbidden(field.NewPath(path), "unknown field"))
	}
	errs = append(errs, validation.ValidateCustomResource(nil, obj, v.schema)...)
	celErrs, _ := v.cel.Validate(ctx, nil, v.s, obj, nil, celconfig.RuntimeCELCostBudget)
	return append(errs, celErrs...), nil
}

// ValidateUpdate returns the CEL rules that refuse changing old into doc
// (transition rules such as immutable fields).
func (v *Validator) ValidateUpdate(ctx context.Context, doc, old []byte) (field.ErrorList, error) {
	obj, prev := map[string]any{}, map[string]any{}
	if err := decode(doc, &obj); err != nil {
		return nil, err
	}
	if err := decode(old, &prev); err != nil {
		return nil, err
	}
	errs, _ := v.cel.Validate(ctx, nil, v.s, obj, prev, celconfig.RuntimeCELCostBudget)
	return errs, nil
}

// Kubernetes unstructured decoding preserves integer values as int64. Generic
// YAML-to-map decoding produces float64, which incorrectly fails CEL int rules.
func decode(doc []byte, obj *map[string]any) error {
	raw, err := yaml.YAMLToJSON(doc)
	if err != nil {
		return err
	}
	return kubejson.Unmarshal(raw, obj)
}
