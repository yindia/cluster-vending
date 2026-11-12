// Package kubestore implements pkg/database/repository on custom
// resources in the management cluster (pkg/apis/platform/v1alpha1)
// instead of a database: one object per row, in one platform namespace,
// named after the row ID. The GORM implementation
// (pkg/database/implementation) is the reference for what every method
// means; both pass the same tests (pkg/database/storetest).
//
// What a database gives and Kubernetes does not is rebuilt from what
// Kubernetes has:
//
//   - a row's etag is a field of the row. Every write is sent with the
//     resourceVersion it was read at, so of two writers of one object one
//     is refused and reads again: no update is lost.
//   - a unique index is a Claim object whose name is derived from the
//     unique value: creating an object is atomic, so one of two creates
//     gets the value. The claim goes with the row.
//   - there are no transactions. A create of a row and its claims undoes
//     itself when a step fails, and WithinTransaction undoes the creates
//     of a failed function; see there for what that leaves out.
//
// The client must read from the API server, not from an informer cache: a
// row is read right after it was written.
package kubestore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/yindia/cluster-vending/pkg/apis/platform/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// Kinds of rows, as a claim names its holder.
const (
	kindCloudAccount = "CloudAccount"
	kindNetwork      = "Network"
	kindCluster      = "Cluster"
	kindNodePool     = "NodePool"
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

// New returns a Store that keeps its rows as objects in namespace. c must
// read from the API server (not from a cache) and know the platform's
// kinds (platformv1alpha1.AddToScheme).
func New(c client.Client, namespace string) repository.Store {
	return store{c: c, ns: namespace}
}

type store struct {
	c  client.Client
	ns string
	// undo, inside WithinTransaction, collects how to take back what was
	// created.
	undo *[]func(context.Context) error
}

func (s store) Tenants() repository.Tenants     { return tenants{s} }
func (s store) Networks() repository.Networks   { return networks{s} }
func (s store) Clusters() repository.Clusters   { return clusters{s} }
func (s store) NodePools() repository.NodePools { return nodePools{s} }

func (s store) ManagedResources() repository.ManagedResources { return managedResources{s} }

// WithinTransaction runs fn and, when it fails, deletes the rows (and
// their claims) fn created, newest first. That is all it can do: there is
// no transaction in Kubernetes. Updates and deletes fn made stay, other
// readers see fn's rows while it runs, and a server that dies inside fn
// leaves what fn did so far.
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

// invalid is a model validation error as the repositories report it.
func invalid(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", repository.ErrInvalid, err)
}

func (s store) meta(name string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: s.ns, Name: name, Labels: labels}
}

// get reads the object named name; ErrNotFound when there is none.
func (s store) get(ctx context.Context, name string, obj client.Object) error {
	if name == "" {
		return repository.ErrNotFound
	}
	return translate(s.c.Get(ctx, client.ObjectKey{Namespace: s.ns, Name: name}, obj))
}

// list lists the objects with every one of labels.
func (s store) list(ctx context.Context, list client.ObjectList, labels map[string]string) error {
	return translate(s.c.List(ctx, list, client.InNamespace(s.ns), client.MatchingLabels(labels)))
}

// remove deletes obj; ErrNotFound when it is gone already.
func (s store) remove(ctx context.Context, obj client.Object) error {
	return translate(s.c.Delete(ctx, obj))
}

// mutate reads the object named name into obj, lets change edit it, and
// writes it back only if nobody wrote it meanwhile; then it starts over.
// change may refuse (its error is returned as it is).
func (s store) mutate(ctx context.Context, name string, obj client.Object, change func() error) error {
	for range writeAttempts {
		if err := s.get(ctx, name, obj); err != nil {
			return err
		}
		if err := change(); err != nil {
			return err
		}
		err := s.c.Update(ctx, obj)
		if !apierrors.IsConflict(err) {
			return translate(err)
		}
	}
	return fmt.Errorf("%w: %s kept changing", repository.ErrConflict, name)
}

// insert creates obj with the claims it needs (key by key, each for the
// row of kind named obj's name), all or nothing: a claim that is taken
// fails the create with ErrAlreadyExists and releases the ones before it.
func (s store) insert(ctx context.Context, kind string, obj client.Object, keys ...string) error {
	for i, key := range keys {
		if err := s.claim(ctx, key, kind, obj.GetName()); err != nil {
			s.release(ctx, keys[:i]...)
			return err
		}
	}
	if err := translate(s.c.Create(ctx, obj)); err != nil {
		s.release(ctx, keys...)
		return err
	}
	if s.undo != nil {
		*s.undo = append(*s.undo, func(ctx context.Context) error {
			err := s.remove(ctx, obj)
			s.release(ctx, keys...)
			return err
		})
	}
	return nil
}

// claimName is the object name of the claim of key.
func claimName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "claim-" + hex.EncodeToString(sum[:20])
}

// claim reserves key for the row holder of kind. ErrAlreadyExists when
// another row holds it. A claim whose holder does not exist and that is
// older than claimGrace was left by a create that died: it is taken.
func (s store) claim(ctx context.Context, key, kind, holder string) error {
	want := &platformv1alpha1.Claim{ObjectMeta: s.meta(claimName(key), nil),
		Spec: platformv1alpha1.ClaimSpec{Key: key, HolderKind: kind, Holder: holder, CreatedAt: metav1.NewMicroTime(now())}}
	for range 2 {
		err := s.c.Create(ctx, want.DeepCopy())
		if !apierrors.IsAlreadyExists(err) {
			return translate(err)
		}
		have := &platformv1alpha1.Claim{}
		if err := s.get(ctx, want.Name, have); err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				continue // released meanwhile
			}
			return err
		}
		if have.Spec.Holder == holder && have.Spec.HolderKind == kind {
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
func (s store) abandoned(ctx context.Context, c *platformv1alpha1.Claim) (bool, error) {
	if now().Sub(c.Spec.CreatedAt.Time) < claimGrace {
		return false, nil
	}
	var holder client.Object
	switch c.Spec.HolderKind {
	case kindCloudAccount:
		holder = &platformv1alpha1.CloudAccount{}
	case kindNetwork:
		holder = &platformv1alpha1.Network{}
	case kindCluster:
		holder = &platformv1alpha1.Cluster{}
	case kindNodePool:
		holder = &platformv1alpha1.NodePool{}
	default:
		return false, nil
	}
	err := s.get(ctx, c.Spec.Holder, holder)
	if errors.Is(err, repository.ErrNotFound) {
		return true, nil
	}
	return false, err
}

// release deletes the claims of keys. Best effort: a claim that stays is
// taken over once it is older than claimGrace (claim).
func (s store) release(ctx context.Context, keys ...string) {
	for _, key := range keys {
		_ = s.c.Delete(ctx, &platformv1alpha1.Claim{ObjectMeta: s.meta(claimName(key), nil)})
	}
}

// holder is the row ID holding the claim of key; ErrNotFound when the
// value is not claimed.
func (s store) holder(ctx context.Context, key string) (string, error) {
	c := &platformv1alpha1.Claim{}
	if err := s.get(ctx, claimName(key), c); err != nil {
		return "", err
	}
	return c.Spec.Holder, nil
}

func newETag() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}

// prepare gives a new row what the database gives it on insert: an ID, an
// etag, generation 1 and its times.
func prepare(m *model.Resource) error {
	if err := m.BeforeCreate(nil); err != nil {
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

// rowAccess reaches the shared columns of a row object, and its tenant
// ("" for kinds that have none).
type rowAccess struct {
	row    func() *platformv1alpha1.Row
	tenant func() string
}

// update writes the row of m (write copies it onto the object) if the
// stored etag is m's, and gives m a new etag. On failure m keeps its old
// etag. The stored creation time is kept.
func (s store) update(ctx context.Context, m *model.Resource, obj client.Object, a rowAccess, write func() error) error {
	old, updated := m.ETag, m.UpdatedAt
	err := s.mutate(ctx, m.ID, obj, func() error {
		if a.row().ETag != old {
			return repository.ErrConflict
		}
		created := a.row().CreatedAt
		m.ETag, m.UpdatedAt = newETag(), now()
		if err := write(); err != nil {
			return err
		}
		a.row().CreatedAt = created
		return nil
	})
	if err != nil {
		m.ETag, m.UpdatedAt = old, updated
	}
	return err
}

// setStatus writes the status only, and only while the row has etag; the
// etag and the update time stay.
func (s store) setStatus(ctx context.Context, tenantID, id, etag string, status model.JSON, obj client.Object, a rowAccess) error {
	raw, err := toJSON(status)
	if err != nil {
		return err
	}
	return s.mutate(ctx, id, obj, func() error {
		switch {
		case a.tenant() != tenantID:
			return repository.ErrNotFound
		case a.row().ETag != etag:
			return repository.ErrConflict
		}
		a.row().Status = raw
		return nil
	})
}

// markBehind puts the applied generation below the generation and assigns
// a new etag, so a write based on the row as it was fails.
func (s store) markBehind(ctx context.Context, tenantID, id string, obj client.Object, a rowAccess) error {
	return s.mutate(ctx, id, obj, func() error {
		if a.tenant() != tenantID {
			return repository.ErrNotFound
		}
		row := a.row()
		row.ETag = newETag()
		if row.AppliedGeneration >= row.Generation {
			row.AppliedGeneration = row.Generation - 1
		}
		return nil
	})
}

// isPending is the background loop's filter (repository.Networks.ListPending).
func isPending(m *model.Resource, states []string, renderTarget int64) bool {
	return slices.Contains(states, m.State) || m.AppliedGeneration < m.Generation || m.RenderVersion < renderTarget
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
