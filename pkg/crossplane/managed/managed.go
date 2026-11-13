// Package managed handles the objects the platform has in the management
// cluster (Crossplane managed resources, Usages, ProviderConfigs,
// namespaces) once they are rendered: reading their state from their
// conditions, deleting them in the order the cloud accepts, and releasing
// Usages nothing uses any more. It knows Kubernetes only: no database, no
// API. Who an object belongs to is the caller's to know (a Ref per
// object).
package managed

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ExternalNameAnnotation holds a managed resource's cloud ID.
const ExternalNameAnnotation = "crossplane.io/external-name"

// waitingOnReference is how Crossplane reports a reference to a resource
// that is not ready yet: the resource waits, it did not fail.
const waitingOnReference = "cannot resolve references"

// PausedAnnotation makes a Crossplane provider stop reconciling a managed
// resource: only the value "true" pauses, and removing it resumes
// (docs.crossplane.io, managed resources, "Paused").
const PausedAnnotation = "crossplane.io/paused"

// ReasonPaused is the reason of a paused resource: what Crossplane puts on
// its Synced condition.
const ReasonPaused = "ReconcilePaused"

// pausedMessage says why a paused resource makes no progress.
const pausedMessage = "reconciliation is paused (crossplane.io/paused): its provider does not act until it is resumed"

// IsPaused reports whether obj carries the paused annotation.
func IsPaused(obj interface{ GetAnnotations() map[string]string }) bool {
	return obj.GetAnnotations()[PausedAnnotation] == "true"
}

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

// Ref names one object in the management cluster.
type Ref struct {
	APIVersion, Kind string
	// Namespace is empty for cluster-scoped objects.
	Namespace, Name string
	// ExternalName is the cloud ID last known for the object, shown while
	// the object is gone. Optional.
	ExternalName string
	// Since is when the caller first knew of the object (SettleMissing).
	// Optional.
	Since time.Time
}

// RefOf is the Ref of a typed object of scheme.
func RefOf(obj client.Object, scheme *runtime.Scheme) (Ref, error) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return Ref{}, fmt.Errorf("%s: %w", obj.GetName(), err)
	}
	apiVersion, kind := gvk.ToAPIVersionAndKind()
	return Ref{APIVersion: apiVersion, Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName()}, nil
}

func (r Ref) key() client.ObjectKey { return client.ObjectKey{Namespace: r.Namespace, Name: r.Name} }

// empty is an object with r's identity and nothing else.
func (r Ref) empty() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
	u.SetNamespace(r.Namespace)
	u.SetName(r.Name)
	return u
}

// Client reads and deletes objects in the management cluster.
type Client struct {
	// Kube reads from the informer cache (a manager's client) and writes to
	// the API server; a plain client works too.
	Kube client.Client
	// Uncached reads from the API server: for objects the cache does not
	// have yet (just applied). Nil: reads use Kube only.
	Uncached client.Reader
}

// Reader is the uncached reader, or Kube without one.
func (c Client) Reader() client.Reader {
	if c.Uncached != nil {
		return c.Uncached
	}
	return c.Kube
}

// ReadError is an object that could not be read (not: one that is gone).
type ReadError struct {
	Ref Ref
	Err error
}

func (e *ReadError) Error() string {
	return fmt.Sprintf("cannot read %s %s from the management cluster: %v", e.Ref.Kind, e.Ref.Name, e.Err)
}

func (e *ReadError) Unwrap() error { return e.Err }

// Observe returns the live view of refs, in their order. An object that
// is gone, or whose kind the cluster does not know, is MISSING; any other
// read failure is a *ReadError.
func (c Client) Observe(ctx context.Context, refs []Ref) ([]*cloudv1.ManagedResource, error) {
	out := make([]*cloudv1.ManagedResource, 0, len(refs))
	for _, r := range refs {
		u, err := c.read(ctx, r)
		switch {
		case err == nil:
			out = append(out, View(r, u))
		case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
			v := base(r)
			v.State, v.Message = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING, "not found in the management cluster"
			out = append(out, v)
		default:
			return nil, &ReadError{Ref: r, Err: err}
		}
	}
	return out, nil
}

// base is a ref's view before reading the object: the known external name
// stays visible when the object is gone.
func base(r Ref) *cloudv1.ManagedResource {
	return &cloudv1.ManagedResource{Kind: r.Kind, Name: r.Name, ExternalName: r.ExternalName}
}

type condition struct {
	status, reason, message string
	observedGeneration      int64
}

// View reads the state of a live object from its conditions:
//
//	DELETING  deletion requested
//	READY     Ready=True; or, for objects that are not managed resources
//	          (namespaces, ProviderConfigs: no spec.forProvider), existing
//	WAITING   Synced=False because a referenced resource is not ready yet;
//	          or paused (PausedAnnotation) and not ready: an operator's
//	          choice, never a failure, however long it lasts
//	FAILED    Synced=False for any other reason, for longer than failureGrace
//	          (before that: CREATING, "retrying: <error>")
//	CREATING  otherwise
func View(r Ref, u *unstructured.Unstructured) *cloudv1.ManagedResource {
	v := base(r)
	v.ExternalName = u.GetAnnotations()[ExternalNameAnnotation]
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
	v.Paused = IsPaused(u)
	switch {
	case u.GetDeletionTimestamp() != nil:
		v.State = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING
		if v.Paused {
			v.Reason, v.Message = ReasonPaused, pausedMessage
		}
	case ready.status == "True" || (!isManaged(u) && !hasReady && !hasSynced):
		v.State = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
	case v.Paused:
		v.State, v.Reason, v.Message = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING, ReasonPaused, pausedMessage
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
		gen, _, _ := unstructured.NestedInt64(m, "observedGeneration")
		out[str("type")] = condition{status: str("status"), reason: str("reason"), message: str("message"),
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

// inGrace reports whether u is younger than failureGrace (see there).
func inGrace(u *unstructured.Unstructured) bool {
	created := u.GetCreationTimestamp().Time
	return !created.IsZero() && now().Sub(created) < failureGrace
}

// AllGone reports whether every object is gone from the cluster (MISSING),
// i.e. a delete finished.
func AllGone(views []*cloudv1.ManagedResource) bool {
	for _, v := range views {
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			return false
		}
	}
	return true
}

// SettleMissing turns MISSING into CREATING for objects known for less
// than failureGrace (Ref.Since; views in ref order, as Observe returns
// them): a just-applied object the reader has not seen yet must not read
// as failed. For resources being created or updated, never for deletions.
func SettleMissing(refs []Ref, views []*cloudv1.ManagedResource) {
	for i, v := range views {
		if i < len(refs) && v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING &&
			now().Sub(refs[i].Since) < failureGrace {
			v.State, v.Message = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING, "not in the management cluster yet"
		}
	}
}

// read returns an object, unstructured: from the cache, else from the API
// server, which has objects the cache has not seen yet (just applied).
func (c Client) read(ctx context.Context, r Ref) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(r.APIVersion, r.Kind))
	return u, c.getUnstructured(ctx, r.key(), u)
}

func (c Client) getUnstructured(ctx context.Context, key client.ObjectKey, u *unstructured.Unstructured) error {
	err := c.Kube.Get(ctx, key, u)
	if apierrors.IsNotFound(err) && c.Uncached != nil {
		err = c.Uncached.Get(ctx, key, u)
	}
	return err
}

// Read is an object as Observe reads it (cache first, then the API
// server); nil when it does not exist.
func (c Client) Read(ctx context.Context, r Ref) (*unstructured.Unstructured, error) {
	u, err := c.read(ctx, r)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil, nil
	}
	return u, err
}

// Get reads a typed object through the cache. The cache holds unstructured
// objects only (one informer per kind), so obj is read unstructured and
// converted; an object the cache does not have (yet) is read from the API
// server.
func (c Client) Get(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	gvk, err := apiutil.GVKForObject(obj, c.Kube.Scheme())
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := c.getUnstructured(ctx, key, u); err != nil {
		return err
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, obj)
}

// List lists typed objects through the cache (see Get).
func (c Client) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	gvk, err := apiutil.GVKForObject(list, c.Kube.Scheme())
	if err != nil {
		return err
	}
	ul := &unstructured.UnstructuredList{}
	ul.SetGroupVersionKind(gvk)
	if err := c.Kube.List(ctx, ul, opts...); err != nil {
		return err
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(ul.UnstructuredContent(), list)
}
