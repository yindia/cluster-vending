package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// externalNameAnnotation holds a managed resource's cloud ID.
const externalNameAnnotation = "crossplane.io/external-name"

// waitingOnReference is how Crossplane reports a reference to a resource
// that is not ready yet: the resource waits, it did not fail.
const waitingOnReference = "cannot resolve references"

// failureGrace is how long after its creation a resource may report
// Synced=False before it counts as FAILED: providers often hit transient
// errors on the first reconciles (e.g. a ProviderConfigUsage race) that
// clear on retry. Until then it is CREATING, with the error as its
// message. It runs from the object's creation, not from the condition's
// lastTransitionTime: AWS errors carry a new request ID on every retry, so
// the condition "changes" each time and that clock never runs out.
const failureGrace = 2 * time.Minute

// now is the clock; tests replace it.
var now = time.Now

// Owner names what objects are created for.
type Owner struct {
	TenantID       string
	CloudAccountID string
	Kind           model.OwnerKind
	ID             string
}

// Record records objects as created for owner, in order. Recording the
// same objects again is a no-op.
func (d Deps) Record(ctx context.Context, owner Owner, objects []client.Object) error {
	rows := make([]model.ManagedResource, 0, len(objects))
	for _, obj := range objects {
		gvk, err := apiutil.GVKForObject(obj, d.Kube.Scheme())
		if err != nil {
			return fmt.Errorf("record %s: %w", obj.GetName(), err)
		}
		apiVersion, kind := gvk.ToAPIVersionAndKind()
		rows = append(rows, model.ManagedResource{
			TenantID: owner.TenantID, CloudAccountID: owner.CloudAccountID, OwnerKind: owner.Kind, OwnerID: owner.ID,
			APIVersion: apiVersion, Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName(),
		})
	}
	return d.Store.ManagedResources().Put(ctx, rows)
}

// Observe returns the live view of recorded objects, in record order.
func (d Deps) Observe(ctx context.Context, records []model.ManagedResource) ([]*cloudv1.ManagedResource, error) {
	out := make([]*cloudv1.ManagedResource, 0, len(records))
	for _, r := range records {
		u, err := d.read(ctx, r)
		switch {
		case err == nil:
			v := view(r, u)
			d.keepExternalName(ctx, r, v.GetExternalName())
			out = append(out, v)
		case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
			v := base(r)
			v.State, v.Message = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING, "not found in the management cluster"
			out = append(out, v)
		default:
			return nil, connect.NewError(connect.CodeUnavailable,
				fmt.Sprintf("cannot read %s %s from the management cluster", r.Kind, r.Name))
		}
	}
	return out, nil
}

// base is a record's view before reading the object: the stored external
// name stays visible when the object is gone.
func base(r model.ManagedResource) *cloudv1.ManagedResource {
	return &cloudv1.ManagedResource{Kind: r.Kind, Name: r.Name, ExternalName: r.ExternalName}
}

// keepExternalName stores an object's external name when it is new or
// changed (once per object, in practice). A failure is logged, not
// returned: the name is stored on a later read.
func (d Deps) keepExternalName(ctx context.Context, r model.ManagedResource, name string) {
	if name == "" || name == r.ExternalName || r.ID == "" {
		return
	}
	if err := d.Store.ManagedResources().SetExternalName(ctx, r.ID, name); err != nil && d.Logger != nil {
		d.Logger.WarnContext(ctx, "store external name", "kind", r.Kind, "name", r.Name, "error", err)
	}
}

// MarkGone stores, for the records of an owner being deleted, when each
// was first seen gone (views in record order, as Observe returns them).
func (d Deps) MarkGone(ctx context.Context, records []model.ManagedResource, views []*cloudv1.ManagedResource) error {
	for i, v := range views {
		if i >= len(records) || records[i].GoneAt != nil || records[i].ID == "" ||
			v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			continue
		}
		if err := d.Store.ManagedResources().SetGoneAt(ctx, records[i].ID, now()); err != nil {
			return fmt.Errorf("store when %s %s went away: %w", records[i].Kind, records[i].Name, err)
		}
	}
	return nil
}

type condition struct {
	status, reason, message string
	observedGeneration      int64
	since                   time.Time // lastTransitionTime; zero when absent
}

// view reads the state of a live object from its conditions:
//
//	DELETING  deletion requested
//	READY     Ready=True; or, for objects that are not managed resources
//	          (namespaces, ProviderConfigs: no spec.forProvider), existing
//	WAITING   Synced=False because a referenced resource is not ready yet
//	FAILED    Synced=False for any other reason, for longer than failureGrace
//	          (before that: CREATING, "retrying: <error>")
//	CREATING  otherwise
func view(r model.ManagedResource, u *unstructured.Unstructured) *cloudv1.ManagedResource {
	v := base(r)
	v.ExternalName = u.GetAnnotations()[externalNameAnnotation]
	if ts := u.GetDeletionTimestamp(); ts != nil {
		v.DeletionTime = timestamppb.New(ts.Time)
	}
	v.Generation = u.GetGeneration()
	v.ZoneId = u.GetLabels()[crossplane.LabelZone]
	v.Role = u.GetLabels()[crossplane.LabelSubnetRole]
	conds := conditions(u)
	ready, hasReady := conds["Ready"]
	synced, hasSynced := conds["Synced"]
	v.Ready, v.Synced = conditionStatus(ready.status), conditionStatus(synced.status)
	v.ObservedGeneration = synced.observedGeneration
	switch {
	case u.GetDeletionTimestamp() != nil:
		v.State = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING
	case ready.status == "True" || (!isManaged(u) && !hasReady && !hasSynced):
		v.State = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
	case synced.status == "False" && strings.Contains(synced.message, waitingOnReference):
		v.State, v.Reason, v.Message = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING, synced.reason, synced.message
	case synced.status == "False" && inGrace(u):
		v.State, v.Reason, v.Message = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING, synced.reason,
			"retrying: "+synced.message
	case synced.status == "False":
		v.State, v.Reason, v.Message = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, synced.reason, synced.message
	default:
		v.State, v.Reason, v.Message = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING, ready.reason, ready.message
	}
	return v
}

// isManaged reports whether u is a Crossplane managed resource: they all
// have spec.forProvider, and no conditions until the provider saw them.
func isManaged(u *unstructured.Unstructured) bool {
	_, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "forProvider")
	return found
}

func conditions(u *unstructured.Unstructured) map[string]condition {
	out := map[string]condition{}
	list, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		str := func(k string) string { s, _ := m[k].(string); return s }
		since, _ := time.Parse(time.RFC3339, str("lastTransitionTime"))
		gen, _, _ := unstructured.NestedInt64(m, "observedGeneration")
		out[str("type")] = condition{status: str("status"), reason: str("reason"), message: str("message"), since: since,
			observedGeneration: gen}
	}
	return out
}

func conditionStatus(s string) cloudv1.ConditionStatus {
	switch s {
	case "True":
		return cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
	case "False":
		return cloudv1.ConditionStatus_CONDITION_STATUS_FALSE
	case "Unknown":
		return cloudv1.ConditionStatus_CONDITION_STATUS_UNKNOWN
	default:
		return cloudv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
	}
}

// usageKind is Crossplane's protection.crossplane.io Usage.
const usageKind = "Usage"

// DeleteRecorded asks the management cluster to delete recorded objects,
// last recorded first (dependents are recorded after what they use).
// Crossplane removes the cloud resources, then the objects. It skips:
//   - Usages: Kubernetes garbage-collects each with the object using
//     (Crossplane owner-references it to spec.by); deleting one first
//     would release its protection at once.
//   - objects already gone.
//
// A 409 is the Usage webhook refusing an object still in use: Crossplane
// records the attempt and replays the delete once the user is gone
// (replayDeletion), so it counts as done. Other errors are collected; the
// remaining objects are still deleted.
func (d Deps) DeleteRecorded(ctx context.Context, records []model.ManagedResource) error {
	var errs []error
	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		if r.Kind == usageKind {
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
		u.SetNamespace(r.Namespace)
		u.SetName(r.Name)
		err := d.Kube.Delete(ctx, u)
		switch {
		case err == nil, apierrors.IsNotFound(err), meta.IsNoMatchError(err), apierrors.IsConflict(err):
		default:
			errs = append(errs, fmt.Errorf("delete %s %s: %w", r.Kind, r.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Prune deletes the objects recorded for an owner that keep no longer
// contains (e.g. the access entry of a removed admin), and forgets them.
func (d Deps) Prune(ctx context.Context, owner Owner, keep []client.Object) error {
	records, err := d.Store.ManagedResources().ListByOwner(ctx, owner.Kind, owner.ID)
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, obj := range keep {
		gvk, err := apiutil.GVKForObject(obj, d.Kube.Scheme())
		if err != nil {
			return err
		}
		kept[gvk.Kind+"/"+obj.GetNamespace()+"/"+obj.GetName()] = true
	}
	for _, r := range records {
		if kept[r.Kind+"/"+r.Namespace+"/"+r.Name] {
			continue
		}
		if err := d.deletePruned(ctx, r); err != nil {
			return err
		}
		if err := d.Store.ManagedResources().Delete(ctx, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// AllGone reports whether every recorded object is gone from the cluster
// (MISSING), i.e. a delete finished.
func AllGone(views []*cloudv1.ManagedResource) bool {
	for _, v := range views {
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			return false
		}
	}
	return true
}

// inGrace reports whether u is younger than failureGrace (see there).
func inGrace(u *unstructured.Unstructured) bool {
	created := u.GetCreationTimestamp().Time
	return !created.IsZero() && now().Sub(created) < failureGrace
}

// read returns a recorded object, unstructured: from the cache, else from
// the API server, which has objects the cache has not seen yet (just
// applied).
func (d Deps) read(ctx context.Context, r model.ManagedResource) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
	return u, d.getUnstructured(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u)
}

func (d Deps) getUnstructured(ctx context.Context, key client.ObjectKey, u *unstructured.Unstructured) error {
	err := d.Kube.Get(ctx, key, u)
	if apierrors.IsNotFound(err) && d.KubeReader != nil {
		err = d.KubeReader.Get(ctx, key, u)
	}
	return err
}

// Get reads a typed object through the cache. The cache holds unstructured
// objects only (one informer per kind), so obj is read unstructured and
// converted; an object the cache does not have (yet) is read from the API
// server.
func (d Deps) Get(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	gvk, err := apiutil.GVKForObject(obj, d.Kube.Scheme())
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := d.getUnstructured(ctx, key, u); err != nil {
		return err
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, obj)
}

// List lists typed objects through the cache (see Get).
func (d Deps) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	gvk, err := apiutil.GVKForObject(list, d.Kube.Scheme())
	if err != nil {
		return err
	}
	ul := &unstructured.UnstructuredList{}
	ul.SetGroupVersionKind(gvk)
	if err := d.Kube.List(ctx, ul, opts...); err != nil {
		return err
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(ul.UnstructuredContent(), list)
}

// SettleMissing turns MISSING into CREATING for objects recorded less than
// failureGrace ago (views in record order, as Observe returns them): a
// just-applied object the reader has not seen yet must not read as failed.
// For resources being created or updated, never for deletions.
func SettleMissing(records []model.ManagedResource, views []*cloudv1.ManagedResource) {
	for i, v := range views {
		if i < len(records) && v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING &&
			now().Sub(records[i].CreatedAt) < failureGrace {
			v.State, v.Message = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING, "not in the management cluster yet"
		}
	}
}

// Reader is the uncached reader, or Kube without one (see Deps.KubeReader).
func (d Deps) Reader() client.Reader {
	if d.KubeReader != nil {
		return d.KubeReader
	}
	return d.Kube
}

// deletePruned deletes one object the render no longer contains. Unlike
// DeleteRecorded it deletes Usages too: a dropped Usage must stop
// protecting its object.
func (d Deps) deletePruned(ctx context.Context, r model.ManagedResource) error {
	if r.Kind != usageKind {
		return d.DeleteRecorded(ctx, []model.ManagedResource{r})
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
	u.SetNamespace(r.Namespace)
	u.SetName(r.Name)
	if err := d.Kube.Delete(ctx, u); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return fmt.Errorf("delete %s %s: %w", r.Kind, r.Name, err)
	}
	return nil
}

// ReadRecorded is a recorded object as Observe reads it (cache first, then
// the API server); nil when it does not exist.
func (d Deps) ReadRecorded(ctx context.Context, r model.ManagedResource) (*unstructured.Unstructured, error) {
	u, err := d.read(ctx, r)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil, nil
	}
	return u, err
}
