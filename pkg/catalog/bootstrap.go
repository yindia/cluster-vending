package catalog

import (
	"encoding/json"
	"fmt"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/naas"
)

// CapsuleBootstrapValues locks this build's shared bootstrap into a NEW release
// pin of the shipped NamespaceTenancy definition (component.RoleNamespaceTenancy). Never use this to augment values read from an already-published Release:
// its chart, templates and authority must move together through release gates.
// The complete platform values snapshot also retains the PDB and system placement.
func CapsuleBootstrapValues(overrides map[string]any) (map[string]any, error) {
	bootstrap, err := naas.Load()
	if err != nil {
		return nil, err
	}
	definition, err := shippedWithRole(component.RoleNamespaceTenancy)
	if err != nil {
		return nil, err
	}
	base := map[string]any{}
	if definition.Spec.PlatformValues != nil {
		if err := json.Unmarshal(definition.Spec.PlatformValues.Raw, &base); err != nil {
			return nil, fmt.Errorf("capsule platform values: %w", err)
		}
	}
	// Helm lists replace. Compose the existing platform PDB and any minor's
	// additional manifests explicitly before appending the canonical bootstrap.
	extra, _ := base["extraManifests"].([]any)
	if additional, exists := overrides["extraManifests"]; exists {
		list, ok := additional.([]any)
		if !ok {
			return nil, fmt.Errorf("capsule extraManifests must be a list")
		}
		extra = append(extra, list...)
	}
	for _, manifest := range bootstrap.HelmExtraManifests() {
		object := manifest.(map[string]any)
		metadata := object["metadata"].(map[string]any)
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
		}
		annotations["cloud.cluster-vending.io/naas-bootstrap-revision"] = bootstrap.Revision()
		metadata["annotations"] = annotations
		extra = append(extra, object)
	}
	identity := map[string]any{"kind": "ServiceAccount", "name": "system:serviceaccount:naas-system:provisioner"}
	// All of these settings belong to the pin, not the mutable definition. Older
	// releases retain their original operator-only values until a release move.
	locked := map[string]any{
		// The chart's object names naas.ObserveCapsuleAvailability looks for.
		"fullnameOverride": naas.CapsuleFullname,
		"serviceAccount":   map[string]any{"create": true, "name": naas.CapsuleServiceAccount},
		"extraManifests":   extra,
		"manager": map[string]any{
			"rbac": map[string]any{
				"create": false, "strict": true, "minimal": false,
				"existingClusterRoles": []any{}, "existingRoles": []any{},
				"bindableClusterRoles": []any{"naas-workload-editor"},
			},
			"options": map[string]any{
				"users": []any{identity}, "administrators": []any{identity},
				"userNames": []any{}, "capsuleUserGroups": []any{},
				"ignoreUserWithGroups": []any{}, "allowServiceAccountPromotion": false,
				"rbac": map[string]any{
					"provisioner": "capsule-namespace-provisioner", "deleter": "capsule-namespace-deleter",
					"administrationClusterRoles": []any{}, "promotionClusterRoles": []any{},
				},
				"impersonation": map[string]any{
					"globalDefaultServiceAccount": "provisioner", "globalDefaultServiceAccountNamespace": "naas-system",
				},
			},
		},
	}
	return component.Merge(nil, base, overrides, locked), nil
}

// shippedWithRole is the shipped definition that declares role.
func shippedWithRole(role component.Role) (*cloudv1alpha1.ComponentDefinition, error) {
	defs, err := Definitions()
	if err != nil {
		return nil, err
	}
	for _, d := range defs {
		if component.Role(d.Spec.Role) == role {
			return d, nil
		}
	}
	return nil, fmt.Errorf("the shipped catalog has no %s definition", role)
}

// ShippedRoleName is the name of the shipped definition that declares
// role; "" when none does.
func ShippedRoleName(role component.Role) string {
	d, err := shippedWithRole(role)
	if err != nil {
		return ""
	}
	return d.Name
}
