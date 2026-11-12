// Package cloudstore implements pkg/database/repository on the platform's
// own kinds in the management cluster (cloud.cluster-vending.io/v1alpha1:
// CloudAccount, Network, Cluster): the platform's only store. The API
// handlers and the reconcilers use it through that interface, and it
// passes the behaviour tests of pkg/database/storetest.
//
// How a row is an object:
//
//   - a tenant is its namespace (kube.TenantNamespace), marked with a
//     label. Accounts, networks and clusters live in it.
//   - metadata.name is the contract's name (an account's is
//     "<provider>-<account ID>"). The row ID, which names the cloud
//     resources and which the API addresses rows by, is a label
//     (LabelID); rows are looked up by it.
//   - spec is the row's contract spec, converted (pkg/convert).
//     The typed spec is the desired state: an edit of it is what the next
//     read returns. What the typed spec has no field for is kept in an
//     annotation (AnnotationRemainder) and handed back on reads.
//   - what the row has and the kind's spec does not (etag, the row's own
//     generations, state, input, adoption, request ID) is in annotations,
//     written together with the spec in one request.
//   - status (a subresource) holds the contract status verbatim
//     (contractStatus), the typed fields read from it, and the owner's
//     managed resource records (inventory).
//   - a Network is owned by its CloudAccount and a Cluster by its Network
//     (controller owner references, pkg/owner).
//
// What a database would give and Kubernetes does not:
//
//   - an etag is an annotation. Every write is sent with the
//     resourceVersion the object was read at, so of two writers one is
//     refused and reads again: no update is lost.
//   - a row update is two requests when its status changed: the object
//     (spec, annotations: everything that decides what is applied), then
//     the status. If the second fails the row has its new etag and its old
//     status; the caller's retry is refused (ErrConflict), reads the row
//     and writes again, and the controller records the status on its next pass.
//   - there are no transactions: WithinTransaction takes back what its
//     function created when it fails, nothing more.
//   - a unique value that is not an object name (a create request ID, the
//     VPC a network was imported from) is a claim: a ConfigMap whose name
//     is derived from the value. Creating an object is atomic, so one of
//     two creates gets the value. The claim goes with the row.
//
// What the rows are:
//
//   - names are unique per tenant: a second network or cluster of a name
//     in the tenant is ErrAlreadyExists (it is the object's name in the
//     tenant's namespace).
//   - AWS accounts only: the kinds have no other provider.
//   - deletes are real: there is no history of deleted rows.
//
// The client must read from the API server, not from an informer cache: a
// row is read right after it was written.
package cloudstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
)

const group = "cloud.cluster-vending.io/"

// Labels of row objects.
const (
	// LabelID is a row's ID (cloud.v1 Network.id, Cluster.id, the bootstrap
	// ID of an account).
	LabelID = group + "id"
	// LabelTenant is the tenant a row belongs to; on a namespace, it marks
	// the namespace as that tenant's.
	LabelTenant = group + "tenant"
	// LabelAccountID is the row ID of the account a network or cluster
	// belongs to.
	LabelAccountID = group + "account-id"
	// LabelNetworkID is the row ID of the network a cluster runs in.
	LabelNetworkID = group + "network-id"
	// LabelClusterID is the row ID of the cluster a component is enabled on.
	LabelClusterID = group + "cluster-id"
	// LabelClaim marks the ConfigMaps that are claims.
	LabelClaim = group + "claim"
)

// Annotations of row objects.
const (
	// AnnotationRow holds what a row has and the kind's spec does not
	// (JSON: etag, generations, state, ...).
	AnnotationRow = group + "row"
	// AnnotationInput is the object as the client asked for it (protojson
	// of the create or import request's resource).
	AnnotationInput = group + "input"
	// AnnotationAdoption is what an imported cluster keeps of the resources
	// it was imported from (JSON crossplaneaws.Adoption).
	AnnotationAdoption = group + "adoption"
	// AnnotationRemainder holds the fields of the contract spec the typed
	// spec has no place for (protojson of a spec with only those fields).
	AnnotationRemainder = group + "spec-remainder"
	// AnnotationDisplayName is a tenant's display name, on its namespace.
	AnnotationDisplayName = group + "display-name"
	// AnnotationCreatedAt is when a tenant was created, on its namespace.
	AnnotationCreatedAt = group + "created-at"
)

// Kinds of rows, as a claim names its holder.
const (
	kindNetwork   = "Network"
	kindCluster   = "Cluster"
	kindComponent = "Component"
)

// writeAttempts bounds how often a write is retried when another writer
// changed the object in between.
const writeAttempts = 8

// claimGrace is how long a claim whose holder does not exist is left
// alone: its holder may be a create that has not stored its row yet.
// Older, it is what a create that died half-way left behind, and the next
// claim of the value takes it.
var claimGrace = time.Minute

// now is the clock; tests replace it.
var now = time.Now

// New returns a Store that keeps its rows as CloudAccount, Network,
// Cluster and Component objects in the tenants' namespaces. c must read from the API
// server (not from a cache) and know the kinds
// (cloudv1alpha1.AddToScheme).
func New(c client.Client) repository.Store { return store{c: c} }

type store struct {
	c client.Client
	// undo, inside WithinTransaction, collects how to take back what was
	// created.
	undo *[]func(context.Context) error
}

func (s store) Tenants() repository.Tenants       { return tenants{s} }
func (s store) Networks() repository.Networks     { return networks{s} }
func (s store) Clusters() repository.Clusters     { return clusters{s} }
func (s store) Components() repository.Components { return components{s} }

func (s store) ManagedResources() repository.ManagedResources { return managedResources{s} }

// WithinTransaction runs fn and, when it fails, deletes what fn created
// (tenants, accounts, networks, clusters, components and their claims),
// newest first.
// That is all it can do: there is no transaction in Kubernetes. Updates
// and deletes fn made stay, other readers see fn's objects while it runs,
// and a server that dies inside fn leaves what fn did so far.
func (s store) WithinTransaction(ctx context.Context, fn func(repository.Store) error) error {
	var undo []func(context.Context) error
	tx := s
	tx.undo = &undo
	err := fn(tx)
	if err == nil {
		return nil
	}
	for i := len(undo) - 1; i >= 0; i-- {
		if uerr := undo[i](ctx); uerr != nil && !errors.Is(uerr, repository.ErrNotFound) {
			err = errors.Join(err, fmt.Errorf("undo: %w", uerr))
		}
	}
	return err
}

// created remembers how to take a create back, inside WithinTransaction.
func (s store) created(undo func(context.Context) error) {
	if s.undo != nil {
		*s.undo = append(*s.undo, undo)
	}
}

// translate maps API errors to repository errors, keeping the cause.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return repository.ErrNotFound
	case apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%w: %w", repository.ErrAlreadyExists, err)
	case apierrors.IsConflict(err):
		return fmt.Errorf("%w: %w", repository.ErrConflict, err)
	case apierrors.IsInvalid(err):
		return fmt.Errorf("%w: %w", repository.ErrInvalid, err)
	default:
		return err
	}
}

// invalid is a validation error as the repositories report it.
func invalid(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", repository.ErrInvalid, err)
}

// onlyAWS refuses the providers the kinds do not have.
func onlyAWS(provider model.Provider) error {
	if err := invalid(provider.Validate()); err != nil {
		return err
	}
	if provider != model.ProviderAWS {
		return fmt.Errorf("%w: the cloud.cluster-vending.io kinds have AWS only, not %s", repository.ErrInvalid, provider)
	}
	return nil
}

// rowMeta is what a row has and its kind's spec does not, as stored in
// AnnotationRow.
type rowMeta struct {
	ETag               string    `json:"etag"`
	State              string    `json:"state,omitempty"`
	Generation         int64     `json:"generation,omitempty"`
	ObservedGeneration int64     `json:"observedGeneration,omitempty"`
	AppliedGeneration  int64     `json:"appliedGeneration,omitempty"`
	RenderVersion      int64     `json:"renderVersion,omitempty"`
	Provider           string    `json:"provider,omitempty"`
	CreateRequestID    *string   `json:"createRequestID,omitempty"`
	ExternalID         *string   `json:"externalID,omitempty"`
	Imported           bool      `json:"imported,omitempty"`
	DeleteForce        bool      `json:"deleteForce,omitempty"`
	ResolvedGeneration int64     `json:"resolvedGeneration,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// metaOf is the row annotation of obj: the zero value for an object that
// has none (one not written by the store).
func metaOf(obj client.Object) rowMeta {
	var m rowMeta
	_ = json.Unmarshal([]byte(obj.GetAnnotations()[AnnotationRow]), &m)
	return m
}

// resourceMeta is the row annotation of a resource.
func resourceMeta(r *model.Resource) rowMeta {
	return rowMeta{ETag: r.ETag, State: r.State, Generation: r.Generation, ObservedGeneration: r.ObservedGeneration,
		AppliedGeneration: r.AppliedGeneration, RenderVersion: r.RenderVersion, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// into copies the row annotation's shared columns onto a resource.
func (m rowMeta) into(r *model.Resource) {
	r.ETag, r.State, r.Generation, r.ObservedGeneration = m.ETag, m.State, m.Generation, m.ObservedGeneration
	r.AppliedGeneration, r.RenderVersion, r.CreatedAt, r.UpdatedAt = m.AppliedGeneration, m.RenderVersion, m.CreatedAt, m.UpdatedAt
}

// annotate sets or, for an empty value, removes annotations of obj.
func annotate(obj client.Object, values map[string]string) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	for k, v := range values {
		if v == "" {
			delete(annotations, k)
		} else {
			annotations[k] = v
		}
	}
	obj.SetAnnotations(annotations)
}

// setMeta writes the row annotation of obj.
func setMeta(obj client.Object, m rowMeta) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	annotate(obj, map[string]string{AnnotationRow: string(raw)})
	return nil
}

// label sets labels of obj, keeping its others.
func label(obj client.Object, values map[string]string) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range values {
		labels[k] = v
	}
	obj.SetLabels(labels)
}

// labelValue reports whether s can be looked up as a label value: an ID
// that cannot is the ID of no row.
func labelValue(s string) bool { return s != "" && len(validation.IsValidLabelValue(s)) == 0 }

// tenantNamespace is the namespace of an existing tenant; ErrNotFound
// when the tenant does not exist (no namespace, or one that is not marked
// as the tenant's).
func (s store) tenantNamespace(ctx context.Context, tenantID string) (*corev1.Namespace, error) {
	if !labelValue(tenantID) {
		return nil, repository.ErrNotFound
	}
	ns := &corev1.Namespace{}
	if err := s.c.Get(ctx, client.ObjectKey{Name: kube.TenantNamespace(tenantID)}, ns); err != nil {
		return nil, translate(err)
	}
	if ns.Labels[LabelTenant] != tenantID {
		return nil, repository.ErrNotFound
	}
	return ns, nil
}

// items lists the objects of list's kind with every one of labels, in
// namespace ("" for all namespaces).
func items[T client.Object](ctx context.Context, s store, list client.ObjectList, namespace string, labels map[string]string) ([]T, error) {
	for _, v := range labels {
		if !labelValue(v) {
			return nil, nil
		}
	}
	if err := s.c.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels(labels)); err != nil {
		return nil, translate(err)
	}
	objects, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(objects))
	for _, o := range objects {
		if typed, ok := o.(T); ok {
			out = append(out, typed)
		}
	}
	return out, nil
}

// byID is the object with the row ID in namespace ("" for all
// namespaces); ErrNotFound when there is none.
func byID[T client.Object](ctx context.Context, s store, list client.ObjectList, namespace, id string) (T, error) {
	var none T
	found, err := items[T](ctx, s, list, namespace, map[string]string{LabelID: id})
	if err != nil {
		return none, err
	}
	if len(found) == 0 {
		return none, repository.ErrNotFound
	}
	return found[0], nil
}

// remove deletes obj as it was read, without its dependents (they are
// never deleted by cascade: the store refuses to delete an owner that has
// any). ErrNotFound when it is gone already, ErrConflict when it changed
// since it was read.
func (s store) remove(ctx context.Context, obj client.Object) error {
	if dropFinalizers(obj) {
		if err := s.c.Update(ctx, obj); err != nil {
			return translate(err)
		}
	}
	version := obj.GetResourceVersion()
	return translate(s.c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationOrphan),
		client.Preconditions{ResourceVersion: &version}))
}

// dropFinalizers removes the kinds' own finalizers from obj and reports
// whether it had any: a row the repository deletes is gone, whatever a
// controller holds it for.
func dropFinalizers(obj client.Object) bool {
	own := []string{cloudv1alpha1.CloudAccountFinalizer, cloudv1alpha1.NetworkFinalizer, cloudv1alpha1.ClusterFinalizer,
		cloudv1alpha1.ComponentFinalizer}
	kept := slices.DeleteFunc(slices.Clone(obj.GetFinalizers()), func(f string) bool { return slices.Contains(own, f) })
	if len(kept) == len(obj.GetFinalizers()) {
		return false
	}
	obj.SetFinalizers(kept)
	return true
}

// claimName is the object name of the claim of key.
func claimName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "cv-claim-" + hex.EncodeToString(sum[:20])
}

// Data keys of a claim.
const (
	claimKey       = "key"
	claimKind      = "holderKind"
	claimHolder    = "holder"
	claimCreatedAt = "createdAt"
)

// claim reserves key in namespace for the row holder (its ID) of kind.
// ErrAlreadyExists when another row holds it. A claim whose holder does
// not exist and that is older than claimGrace was left by a create that
// died: it is taken.
func (s store) claim(ctx context.Context, namespace, key, kind, holder string) error {
	want := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: claimName(key), Labels: map[string]string{LabelClaim: "true"}},
		Data: map[string]string{claimKey: key, claimKind: kind, claimHolder: holder,
			claimCreatedAt: now().UTC().Format(time.RFC3339Nano)},
	}
	for range 2 {
		err := s.c.Create(ctx, want.DeepCopy())
		if !apierrors.IsAlreadyExists(err) {
			return translate(err)
		}
		have := &corev1.ConfigMap{}
		if err := s.c.Get(ctx, client.ObjectKeyFromObject(want), have); err != nil {
			if apierrors.IsNotFound(err) {
				continue // released meanwhile
			}
			return translate(err)
		}
		if have.Data[claimHolder] == holder && have.Data[claimKind] == kind {
			return nil
		}
		abandoned, err := s.abandoned(ctx, have)
		if err != nil {
			return err
		}
		if !abandoned {
			break
		}
		// Only the claim as it was read: one taken meanwhile stays.
		err = s.c.Delete(ctx, have, client.Preconditions{UID: &have.UID, ResourceVersion: &have.ResourceVersion})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return translate(err)
		}
	}
	return fmt.Errorf("%w: %s", repository.ErrAlreadyExists, key)
}

// abandoned reports whether a claim's holder does not exist although the
// claim is older than claimGrace.
func (s store) abandoned(ctx context.Context, c *corev1.ConfigMap) (bool, error) {
	created, err := time.Parse(time.RFC3339Nano, c.Data[claimCreatedAt])
	if err == nil && now().Sub(created) < claimGrace {
		return false, nil
	}
	switch c.Data[claimKind] {
	case kindNetwork:
		_, err = byID[*cloudv1alpha1.Network](ctx, s, &cloudv1alpha1.NetworkList{}, c.Namespace, c.Data[claimHolder])
	case kindCluster:
		_, err = byID[*cloudv1alpha1.Cluster](ctx, s, &cloudv1alpha1.ClusterList{}, c.Namespace, c.Data[claimHolder])
	case kindComponent:
		_, err = byID[*cloudv1alpha1.Component](ctx, s, &cloudv1alpha1.ComponentList{}, c.Namespace, c.Data[claimHolder])
	default:
		return false, nil
	}
	if errors.Is(err, repository.ErrNotFound) {
		return true, nil
	}
	return false, err
}

// release deletes the claims of keys. Best effort: a claim that stays is
// taken over once it is older than claimGrace (claim).
func (s store) release(ctx context.Context, namespace string, keys ...string) {
	for _, key := range keys {
		_ = s.c.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: claimName(key)}})
	}
}

// claimAll claims keys, all or nothing: a key that is taken fails with
// ErrAlreadyExists and releases the ones before it.
func (s store) claimAll(ctx context.Context, namespace, kind, holder string, keys []string) error {
	for i, key := range keys {
		if err := s.claim(ctx, namespace, key, kind, holder); err != nil {
			s.release(ctx, namespace, keys[:i]...)
			return err
		}
	}
	return nil
}

// holder is the row ID holding the claim of key; ErrNotFound when the
// value is not claimed.
func (s store) holder(ctx context.Context, namespace, key string) (string, error) {
	c := &corev1.ConfigMap{}
	if err := s.c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: claimName(key)}, c); err != nil {
		return "", translate(err)
	}
	return c.Data[claimHolder], nil
}

func newETag() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}

// prepare gives a new row what a store gives it on insert: an ID, an
// etag, generation 1 and its times.
func prepare(m *model.Resource) error {
	if err := m.AssignID(); err != nil {
		return err
	}
	m.ETag = newETag()
	if m.Generation == 0 {
		m.Generation = 1
	}
	m.CreatedAt = now()
	m.UpdatedAt = m.CreatedAt
	return nil
}

// page cuts rows (sorted by key) to one page.
func page[T any](rows []T, key func(T) string, p repository.Page) ([]T, error) {
	if p.Limit <= 0 {
		return nil, fmt.Errorf("page limit must be positive, got %d", p.Limit)
	}
	out := make([]T, 0, min(p.Limit, len(rows)))
	for _, row := range rows {
		if p.After != "" && key(row) <= p.After {
			continue
		}
		if len(out) == p.Limit {
			break
		}
		out = append(out, row)
	}
	return out, nil
}

// sameJSON reports whether two documents say the same: the API server
// stores JSON by meaning, not byte for byte.
func sameJSON(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(x, y)
}

// retry runs write until it is not refused because the object changed
// meanwhile (a Kubernetes conflict); write reads the object again every
// time. ErrConflict when it kept changing.
func retry(write func() error) error {
	for range writeAttempts {
		err := write()
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("%w: the object kept changing", repository.ErrConflict)
}
