package cloudstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/owner"
)

// components keeps each component as a Component object in the tenant
// namespace, named <cluster name>-<component> and owned by its Cluster
// object: the name makes a component unique per cluster.
type components struct{ store }

// clusterDeleting is the stored state of a cluster being deleted.
const clusterDeleting = "CLUSTER_STATE_DELETING"

// componentKeys are the claims of a component: its create request ID.
func componentKeys(c *model.Component) []string {
	if c.CreateRequestID == nil {
		return nil
	}
	return []string{"component-request/" + *c.CreateRequestID}
}

// componentValues are a row's values as the kind keeps them: nil for
// none; ErrInvalid unless they are a JSON object of at most
// MaxComponentValuesBytes.
func componentValues(raw model.JSON) (*runtime.RawExtension, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if len(trimmed) > cloudv1alpha1.MaxComponentValuesBytes {
		return nil, fmt.Errorf("%w: values are %d bytes, at most %d", repository.ErrInvalid,
			len(trimmed), cloudv1alpha1.MaxComponentValuesBytes)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%w: values are not a JSON object", repository.ErrInvalid)
	}
	return &runtime.RawExtension{Raw: bytes.Clone(trimmed)}, nil
}

// fill writes the row onto obj: spec, labels and the row annotation,
// everything but the status.
func (r components) fill(c *model.Component, obj *cloudv1alpha1.Component) error {
	values, err := componentValues(c.Values)
	if err != nil {
		return err
	}
	obj.Spec = cloudv1alpha1.ComponentSpec{ClusterID: c.ClusterID, Component: c.Component, Version: c.Version, Values: values}
	label(obj, map[string]string{LabelID: c.ID, LabelTenant: c.TenantID, LabelClusterID: c.ClusterID})
	return setMeta(obj, rowMeta{ETag: c.ETag, State: c.State, Generation: c.Generation,
		ObservedGeneration: c.ObservedGeneration, CreateRequestID: c.CreateRequestID, DeleteForce: c.DeleteForce,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
}

// row is the row an object holds.
func (r components) row(obj *cloudv1alpha1.Component) (*model.Component, error) {
	m := metaOf(obj)
	c := &model.Component{
		Base:     model.Base{ID: obj.Labels[LabelID], CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		TenantID: obj.Labels[LabelTenant], ClusterID: obj.Spec.ClusterID, Component: obj.Spec.Component,
		Version: obj.Spec.Version, State: m.State, Generation: m.Generation, ObservedGeneration: m.ObservedGeneration,
		ETag: m.ETag, CreateRequestID: m.CreateRequestID, DeleteForce: m.DeleteForce,
	}
	if v := obj.Spec.Values; v != nil && len(v.Raw) > 0 {
		c.Values = model.JSON(bytes.Clone(v.Raw))
	}
	status := convert.ComponentStatusFromAPI(&obj.Status)
	if proto.Size(status) > 0 {
		raw, err := protojson.Marshal(status)
		if err != nil {
			return nil, fmt.Errorf("component %s: status: %w", obj.Name, err)
		}
		c.Status = raw
	}
	return c, nil
}

// setStatus writes a row's status onto obj.Status. The inventory stays.
func (r components) setStatus(obj *cloudv1alpha1.Component, raw model.JSON, state string) {
	p := &cloudv1.ComponentStatus{}
	_ = decode(raw, p) // a status this build cannot read leaves the typed fields empty
	convert.ComponentStatusInto(p, state, metav1.NewTime(now()), &obj.Status)
	obj.Status.ObservedGeneration = p.GetObservedGeneration()
}

// find is the component of the tenant with the row ID.
func (r components) find(ctx context.Context, tenantID, id string) (*cloudv1alpha1.Component, error) {
	if !labelValue(tenantID) {
		return nil, repository.ErrNotFound
	}
	return byID[*cloudv1alpha1.Component](ctx, r.store, &cloudv1alpha1.ComponentList{}, kube.TenantNamespace(tenantID), id)
}

// Create stores a component as a Component object owned by its cluster,
// and claims the cluster: the cluster must not be DELETING, and its etag
// changes, so of a create and a cluster delete that race exactly one wins
// (clusters.Create does the same with the network).
func (r components) Create(ctx context.Context, c *model.Component) error {
	cluster, err := clusters(r).find(ctx, c.TenantID, c.ClusterID)
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	if metaOf(cluster).State == clusterDeleting {
		return fmt.Errorf("%w: cluster %s is being deleted", repository.ErrConflict, cluster.Name)
	}
	if err := c.AssignID(); err != nil {
		return err
	}
	c.ETag, c.CreatedAt = newETag(), now()
	c.UpdatedAt = c.CreatedAt
	if c.Generation == 0 {
		c.Generation = 1
	}
	obj := &cloudv1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: cluster.Name + "-" + c.Component}}
	if err := r.fill(c, obj); err != nil {
		return err
	}
	if err := owner.Set(cluster, obj, r.c.Scheme()); err != nil {
		return invalid(err)
	}
	keys := componentKeys(c)
	if err := r.claimAll(ctx, obj.Namespace, kindComponent, c.ID, keys); err != nil {
		return err
	}
	takeBack := func(ctx context.Context) error {
		err := r.c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationOrphan))
		r.release(ctx, obj.Namespace, keys...)
		return translate(err)
	}
	if err := r.insert(ctx, obj, func() { r.setStatus(obj, c.Status, c.State) }); err != nil {
		r.release(ctx, obj.Namespace, keys...)
		return err
	}
	if err := r.claimCluster(ctx, c.TenantID, c.ClusterID); err != nil {
		if terr := takeBack(ctx); terr != nil && !errors.Is(terr, repository.ErrNotFound) {
			return errors.Join(err, fmt.Errorf("take the component back: %w", terr))
		}
		return err
	}
	r.created(takeBack)
	return nil
}

// claimCluster gives the cluster a new etag unless it is DELETING
// (ErrConflict) or gone (ErrNotFound): clusters.claimNetwork for a
// cluster.
func (r components) claimCluster(ctx context.Context, tenantID, id string) error {
	err := translate(retry(func() error {
		cluster, err := clusters(r).find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		m := metaOf(cluster)
		if m.State == clusterDeleting {
			return fmt.Errorf("%w: cluster %s is being deleted", repository.ErrConflict, cluster.Name)
		}
		m.ETag = newETag()
		if err := setMeta(cluster, m); err != nil {
			return err
		}
		return r.c.Update(ctx, cluster)
	}))
	if errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("cluster: %w", err)
	}
	return err
}

func (r components) Get(ctx context.Context, tenantID, id string) (*model.Component, error) {
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return r.row(obj)
}

func (r components) GetByCreateRequest(ctx context.Context, tenantID, requestID string) (*model.Component, error) {
	if !labelValue(tenantID) {
		return nil, repository.ErrNotFound
	}
	id, err := r.holder(ctx, kube.TenantNamespace(tenantID), "component-request/"+requestID)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, tenantID, id)
}

// all lists the components with labels in namespace, ordered by ID
// (oldest first).
func (r components) all(ctx context.Context, namespace string, labels map[string]string) ([]model.Component, error) {
	objects, err := items[*cloudv1alpha1.Component](ctx, r.store, &cloudv1alpha1.ComponentList{}, namespace, labels)
	if err != nil {
		return nil, err
	}
	out := make([]model.Component, 0, len(objects))
	for _, obj := range objects {
		if obj.Labels[LabelID] == "" {
			continue // not a row: an object the store did not write
		}
		c, err := r.row(obj)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r components) List(ctx context.Context, tenantID string, p repository.Page) ([]model.Component, error) {
	if p.Limit <= 0 {
		return nil, fmt.Errorf("page limit must be positive, got %d", p.Limit)
	}
	rows, err := r.all(ctx, kube.TenantNamespace(tenantID), map[string]string{LabelTenant: tenantID})
	if err != nil {
		return nil, err
	}
	return page(rows, func(c model.Component) string { return c.ID }, p)
}

func (r components) ListByCluster(ctx context.Context, tenantID, clusterID string) ([]model.Component, error) {
	return r.all(ctx, kube.TenantNamespace(tenantID), map[string]string{LabelTenant: tenantID, LabelClusterID: clusterID})
}

// Update writes the row if the stored etag is c's, and gives c a new one.
// The cluster and the component are the object's identity: a row that
// changes either is refused.
func (r components) Update(ctx context.Context, c *model.Component) error {
	old, updated := c.ETag, c.UpdatedAt
	var obj *cloudv1alpha1.Component
	err := retry(func() error {
		var err error
		if obj, err = r.find(ctx, c.TenantID, c.ID); err != nil {
			return err
		}
		m := metaOf(obj)
		if m.ETag != old {
			return repository.ErrConflict
		}
		if c.ClusterID != obj.Spec.ClusterID || c.Component != obj.Spec.Component {
			return fmt.Errorf("%w: the cluster and the component of a component cannot change", repository.ErrInvalid)
		}
		c.ETag, c.UpdatedAt, c.CreatedAt = newETag(), now(), m.CreatedAt
		if err := r.fill(c, obj); err != nil {
			return err
		}
		return r.c.Update(ctx, obj)
	})
	if err != nil {
		c.ETag, c.UpdatedAt = old, updated
		return translate(err)
	}
	return r.writeStatus(ctx, c.TenantID, c.ID, c.ETag, c.Status, obj)
}

// writeStatus is clusters.writeStatus for a component.
func (r components) writeStatus(ctx context.Context, tenantID, id, etag string, status model.JSON, obj *cloudv1alpha1.Component) error {
	return translate(retry(func() error {
		if obj == nil {
			var err error
			if obj, err = r.find(ctx, tenantID, id); err != nil {
				return err
			}
		}
		current := obj
		obj = nil // read again on a retry
		m := metaOf(current)
		if m.ETag != etag {
			return nil
		}
		before := current.Status.DeepCopy()
		r.setStatus(current, status, m.State)
		if equality.Semantic.DeepEqual(before, &current.Status) {
			return nil
		}
		return r.c.Status().Update(ctx, current)
	}))
}

// SetStatus writes the status only, and only while the row has etag; the
// etag and the update time stay.
func (r components) SetStatus(ctx context.Context, tenantID, id, etag string, status model.JSON) error {
	return translate(retry(func() error {
		obj, err := r.find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		m := metaOf(obj)
		if m.ETag != etag {
			return repository.ErrConflict
		}
		before := obj.Status.DeepCopy()
		r.setStatus(obj, status, m.State)
		if equality.Semantic.DeepEqual(before, &obj.Status) {
			return nil
		}
		return r.c.Status().Update(ctx, obj)
	}))
}

// Delete removes the component.
func (r components) Delete(ctx context.Context, tenantID, id string) error {
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		return err
	}
	row, err := r.row(obj)
	if err != nil {
		return err
	}
	if err := r.remove(ctx, obj); err != nil {
		return err
	}
	r.release(ctx, obj.Namespace, componentKeys(row)...)
	return nil
}
