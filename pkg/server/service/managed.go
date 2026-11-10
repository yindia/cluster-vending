package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// failureGrace is how long a resource may report Synced=False before it
// counts as FAILED: providers often hit transient errors on the first
// reconciles (e.g. a ProviderConfigUsage race) that clear on retry. Until
// then it is CREATING, with the error as its message.
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
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
		err := d.Kube.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u)
		switch {
		case err == nil:
			out = append(out, view(r, u))
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

func base(r model.ManagedResource) *cloudv1.ManagedResource {
	return &cloudv1.ManagedResource{Kind: r.Kind, Name: r.Name}
}

type condition struct {
	status, reason, message string
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
	v.ZoneId = u.GetLabels()[crossplane.LabelZone]
	v.Role = u.GetLabels()[crossplane.LabelSubnetRole]
	conds := conditions(u)
	ready, hasReady := conds["Ready"]
	synced, hasSynced := conds["Synced"]
	v.Ready, v.Synced = conditionStatus(ready.status), conditionStatus(synced.status)
	switch {
	case u.GetDeletionTimestamp() != nil:
		v.State = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING
	case ready.status == "True" || (!isManaged(u) && !hasReady && !hasSynced):
		v.State = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
	case synced.status == "False" && strings.Contains(synced.message, waitingOnReference):
		v.State, v.Reason, v.Message = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING, synced.reason, synced.message
	case synced.status == "False" && !synced.since.IsZero() && now().Sub(synced.since) < failureGrace:
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
		out[str("type")] = condition{status: str("status"), reason: str("reason"), message: str("message"), since: since}
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

// DeleteRecorded asks the cluster to delete recorded objects; Crossplane
// removes the cloud resources and then the objects. Objects already gone
// are skipped.
func (d Deps) DeleteRecorded(ctx context.Context, records []model.ManagedResource) error {
	for _, r := range records {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
		u.SetNamespace(r.Namespace)
		u.SetName(r.Name)
		if err := d.Kube.Delete(ctx, u); err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return fmt.Errorf("delete %s %s: %w", r.Kind, r.Name, err)
		}
	}
	return nil
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
		if err := d.DeleteRecorded(ctx, []model.ManagedResource{r}); err != nil {
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
