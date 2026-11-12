package cluster

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// Keys of the stored adoption (a JSON object) under which an imported
// cluster keeps the objects its import planned: its Observe-only managed
// resources, exactly as they are applied, and the cloud IDs of its pod
// identity associations. The rest of the adoption is
// crossplaneaws.Adoption; readers of that skip the keys.
const (
	importObjectsKey       = "importObjects"
	importPodIdentitiesKey = "importPodIdentities"
)

// WithImportObjects is adoption (crossplaneaws.Adoption as stored JSON)
// with the objects of the import plan kept next to it. ImportCluster
// stores it; the reconciler applies the objects from it, so the cloud is
// looked up once, by the call.
func WithImportObjects(adoption model.JSON, plan corecluster.ImportPlan, scheme *runtime.Scheme) (model.JSON, error) {
	fields := map[string]json.RawMessage{}
	if len(adoption) > 0 {
		if err := json.Unmarshal(adoption, &fields); err != nil {
			return nil, fmt.Errorf("stored adoption: %w", err)
		}
	}
	bodies := make([]*unstructured.Unstructured, 0, len(plan.Objects))
	for _, obj := range plan.Objects {
		body, err := kube.ApplyObject(obj, scheme)
		if err != nil {
			return nil, fmt.Errorf("encode %T %s: %w", obj, obj.GetName(), err)
		}
		bodies = append(bodies, body)
	}
	objects, err1 := json.Marshal(bodies)
	identities, err2 := json.Marshal(plan.PodIdentities)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("encode the import's objects: %w", firstError(err1, err2))
	}
	fields[importObjectsKey], fields[importPodIdentitiesKey] = objects, identities
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode the adoption: %w", err)
	}
	return model.JSON(out), nil
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// storedImportObjects are the objects an import kept with the row, as
// typed objects of scheme, and the cloud IDs of its pod identity
// associations by object name; false when there are none (a cluster the
// platform created, or one imported before they were kept: its objects
// exist already).
func storedImportObjects(row *model.Cluster, scheme *runtime.Scheme) ([]client.Object, map[string]string, bool, error) {
	if len(row.Adoption) == 0 {
		return nil, nil, false, nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(row.Adoption, &fields); err != nil {
		return nil, nil, false, fmt.Errorf("stored adoption: %w", err)
	}
	raw, ok := fields[importObjectsKey]
	if !ok {
		return nil, nil, false, nil
	}
	var bodies []*unstructured.Unstructured
	if err := json.Unmarshal(raw, &bodies); err != nil {
		return nil, nil, false, fmt.Errorf("stored import objects: %w", err)
	}
	objects := make([]client.Object, 0, len(bodies))
	for _, body := range bodies {
		obj, err := typed(body, scheme)
		if err != nil {
			return nil, nil, false, err
		}
		objects = append(objects, obj)
	}
	identities := map[string]string{}
	if raw, ok := fields[importPodIdentitiesKey]; ok {
		if err := json.Unmarshal(raw, &identities); err != nil {
			return nil, nil, false, fmt.Errorf("stored pod identity IDs: %w", err)
		}
	}
	return objects, identities, true, nil
}

// typed is body as the object of its kind in scheme: what the renderer
// made, so it is applied exactly as an import applied it before.
func typed(body *unstructured.Unstructured, scheme *runtime.Scheme) (client.Object, error) {
	made, err := scheme.New(body.GroupVersionKind())
	if err != nil {
		return nil, fmt.Errorf("stored import object %s %s: %w", body.GetKind(), body.GetName(), err)
	}
	obj, ok := made.(client.Object)
	if !ok {
		return nil, fmt.Errorf("stored import object %s %s is not an object", body.GetKind(), body.GetName())
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(body.Object, obj); err != nil {
		return nil, fmt.Errorf("stored import object %s %s: %w", body.GetKind(), body.GetName(), err)
	}
	return obj, nil
}

// applyStoredImport creates the Observe-only managed resources of an
// imported cluster from the objects its import planned (kept with the
// row). Nothing is looked up in the cloud again. It is safe to repeat.
func (s *Reconciler) applyStoredImport(ctx context.Context, row *model.Cluster) error {
	objects, identities, ok, err := storedImportObjects(row, s.deps.Kube.Scheme())
	if err != nil || !ok {
		return err
	}
	account, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, row.TenantID, row.CloudAccountID)
	if err != nil {
		return storeError(err)
	}
	return s.applyImport(ctx, row, account, objects, identities)
}
