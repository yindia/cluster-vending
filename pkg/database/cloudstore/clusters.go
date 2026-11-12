package cloudstore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/owner"
)

type clusters struct{ store }

// Stored states of a cluster the store itself looks at.
const (
	clusterImported = "CLUSTER_STATE_IMPORTED"
	// clusterBlocked is the stored state of an import that was blocked.
	clusterBlocked = "CLUSTER_STATE_FAILED"
)

// clusterKeys are the claims of a cluster: its create request ID. Its
// name is the object's name, unique in the tenant by itself.
func clusterKeys(c *model.Cluster) []string {
	if c.CreateRequestID == nil {
		return nil
	}
	return []string{"cluster-request/" + *c.CreateRequestID}
}

// clusterImport is spec.importFrom of a cluster row: nil for a cluster
// the platform created (networkImport, for a cluster).
func clusterImport(c *model.Cluster, stored *cloudv1alpha1.ClusterImport) *cloudv1alpha1.ClusterImport {
	out := stored.DeepCopy()
	req := &cloudv1.ImportClusterRequest{}
	if err := decode(c.Input, req); err == nil && req.GetAws().GetDefaultNodeGroupName() != "" {
		out = &cloudv1alpha1.ClusterImport{
			AWS: &cloudv1alpha1.AWSImportCluster{DefaultNodeGroupName: req.GetAws().GetDefaultNodeGroupName()}}
	}
	if out == nil {
		return nil
	}
	takenOver := c.State != clusterImported && c.State != clusterBlocked || stored != nil && stored.TakeOver
	out.Takeover = cloudv1alpha1.Takeover{TakeOver: takenOver, AcknowledgeNotImported: takenOver}
	return out
}

// fill writes the row onto obj: spec, labels and annotations, everything
// but the status. networkName is the Network object of the row's network.
func (r clusters) fill(c *model.Cluster, networkName string, obj *cloudv1alpha1.Cluster) error {
	p := &cloudv1.ClusterSpec{}
	if err := decode(c.Spec, p); err != nil {
		return fmt.Errorf("%w: cluster spec: %w", repository.ErrInvalid, err)
	}
	spec, remainder := convert.ClusterSpecToAPI(p, networkName)
	spec.DisplayName, spec.Tags, spec.ImportFrom = c.DisplayName, cloudv1alpha1.Tags(c.Labels), clusterImport(c, obj.Spec.ImportFrom)
	if spec.Region == "" {
		spec.Region = c.Region
	}
	obj.Spec = spec
	label(obj, map[string]string{LabelID: c.ID, LabelTenant: c.TenantID, LabelAccountID: c.CloudAccountID, LabelNetworkID: c.NetworkID})
	rest, err := remainderJSON(remainder != nil, remainder)
	if err != nil {
		return err
	}
	annotate(obj, map[string]string{AnnotationInput: string(c.Input), AnnotationRemainder: rest, AnnotationAdoption: string(c.Adoption)})
	m := resourceMeta(&c.Resource)
	m.Provider, m.CreateRequestID, m.Imported, m.DeleteForce = string(c.Provider), c.CreateRequestID, c.Imported, c.DeleteForce
	m.ResolvedGeneration = c.ResolvedGeneration
	return setMeta(obj, m)
}

// row is the row an object holds.
func (r clusters) row(obj *cloudv1alpha1.Cluster) (*model.Cluster, error) {
	remainder := &cloudv1.ClusterSpec{}
	if err := decode([]byte(obj.Annotations[AnnotationRemainder]), remainder); err != nil {
		return nil, fmt.Errorf("cluster %s: stored spec remainder: %w", obj.Name, err)
	}
	spec, err := protojson.Marshal(convert.ClusterSpecFromAPI(obj.Spec, obj.Labels[LabelNetworkID], remainder))
	if err != nil {
		return nil, err
	}
	m := metaOf(obj)
	c := &model.Cluster{
		Resource: model.Resource{Base: model.Base{ID: obj.Labels[LabelID]}, DisplayName: obj.Spec.DisplayName,
			Labels: model.Labels(obj.Spec.Tags), Spec: spec, Status: contractStatus(obj.Status.ContractStatus)},
		TenantID: obj.Labels[LabelTenant], Name: obj.Name, Input: model.JSON(obj.Annotations[AnnotationInput]),
		NetworkID: obj.Labels[LabelNetworkID], CloudAccountID: obj.Labels[LabelAccountID],
		Provider: model.Provider(m.Provider), Region: obj.Spec.Region, Imported: m.Imported,
		CreateRequestID: m.CreateRequestID, DeleteForce: m.DeleteForce, ResolvedGeneration: m.ResolvedGeneration,
	}
	if adoption := obj.Annotations[AnnotationAdoption]; adoption != "" {
		c.Adoption = model.JSON(adoption)
	}
	m.into(&c.Resource)
	return c, nil
}

// setStatus writes a row's status onto obj.Status: the contract status
// verbatim, and the typed fields read from it. The inventory stays.
func (r clusters) setStatus(obj *cloudv1alpha1.Cluster, raw model.JSON, state string) {
	p := &cloudv1.ClusterStatus{}
	_ = decode(raw, p) // a status this build cannot read is still kept verbatim
	st := &obj.Status
	st.ContractStatus = keepStatus(st.ContractStatus, raw)
	convert.ClusterStatusInto(p, state, metav1.NewTime(now()), st)
	st.ObservedGeneration = obj.Generation
}

// find is the cluster of the tenant with the row ID.
func (r clusters) find(ctx context.Context, tenantID, id string) (*cloudv1alpha1.Cluster, error) {
	if !labelValue(tenantID) {
		return nil, repository.ErrNotFound
	}
	return byID[*cloudv1alpha1.Cluster](ctx, r.store, &cloudv1alpha1.ClusterList{}, kube.TenantNamespace(tenantID), id)
}

// network is the Network object of the row's network: ErrNotFound when
// there is none, ErrInvalid when it is another tenant's.
func (r clusters) network(ctx context.Context, c *model.Cluster) (*cloudv1alpha1.Network, error) {
	network, err := byID[*cloudv1alpha1.Network](ctx, r.store, &cloudv1alpha1.NetworkList{}, "", c.NetworkID)
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	if tenant := network.Labels[LabelTenant]; tenant != c.TenantID {
		return nil, fmt.Errorf("%w: network %s belongs to tenant %s, cluster to tenant %s",
			repository.ErrInvalid, network.Name, tenant, c.TenantID)
	}
	return network, nil
}

// Create stores a cluster as a Cluster object owned by its network, and
// claims the network: the network must not be DELETING, and its etag
// changes. DeleteNetwork marks the network DELETING with the etag it read
// before it checked for clusters, so of a create and a delete that race
// exactly one wins.
//
// Without a transaction the order does the work: the cluster is stored
// first, then the network is claimed. A delete that looked for clusters
// before this one was stored still holds the old etag and is refused; a
// delete that already won leaves the network DELETING (or gone), which
// the claim sees, and the cluster is taken back.
func (r clusters) Create(ctx context.Context, c *model.Cluster) error {
	network, err := r.network(ctx, c)
	if err != nil {
		return err
	}
	if err := onlyAWS(c.Provider); err != nil {
		return err
	}
	if metaOf(network).State == networkDeleting {
		return fmt.Errorf("%w: network %s is being deleted", repository.ErrConflict, network.Name)
	}
	// The network's account is the cluster's.
	c.CloudAccountID = network.Labels[LabelAccountID]
	if err := prepare(&c.Resource); err != nil {
		return err
	}
	obj := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: network.Namespace, Name: c.Name}}
	if err := r.fill(c, network.Name, obj); err != nil {
		return err
	}
	if err := owner.Set(network, obj, r.c.Scheme()); err != nil {
		return invalid(err)
	}
	keys := clusterKeys(c)
	if err := r.claimAll(ctx, obj.Namespace, kindCluster, c.ID, keys); err != nil {
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
	if err := r.claimNetwork(ctx, c.TenantID, c.NetworkID); err != nil {
		if terr := takeBack(ctx); terr != nil && !errors.Is(terr, repository.ErrNotFound) {
			return errors.Join(err, fmt.Errorf("take the cluster back: %w", terr))
		}
		return err
	}
	r.created(takeBack)
	return nil
}

// claimNetwork gives the network a new etag unless it is DELETING
// (ErrConflict) or gone (ErrNotFound). It is not conditional on an etag
// read before: two creates in one network, or the loop touching the
// network meanwhile, must not fail each other.
func (r clusters) claimNetwork(ctx context.Context, tenantID, id string) error {
	err := translate(retry(func() error {
		network, err := networks(r).find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		m := metaOf(network)
		if m.State == networkDeleting {
			return fmt.Errorf("%w: network %s is being deleted", repository.ErrConflict, network.Name)
		}
		m.ETag = newETag()
		if err := setMeta(network, m); err != nil {
			return err
		}
		return r.c.Update(ctx, network)
	}))
	if errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("network: %w", err)
	}
	return err
}

func (r clusters) Get(ctx context.Context, tenantID, id string) (*model.Cluster, error) {
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return r.row(obj)
}

func (r clusters) GetByCreateRequest(ctx context.Context, tenantID, requestID string) (*model.Cluster, error) {
	if !labelValue(tenantID) {
		return nil, repository.ErrNotFound
	}
	id, err := r.holder(ctx, kube.TenantNamespace(tenantID), "cluster-request/"+requestID)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, tenantID, id)
}

// all lists the clusters with labels in namespace, ordered by ID (oldest
// first).
func (r clusters) all(ctx context.Context, namespace string, labels map[string]string) ([]model.Cluster, error) {
	objects, err := items[*cloudv1alpha1.Cluster](ctx, r.store, &cloudv1alpha1.ClusterList{}, namespace, labels)
	if err != nil {
		return nil, err
	}
	out := make([]model.Cluster, 0, len(objects))
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

func (r clusters) List(ctx context.Context, tenantID string, p repository.Page) ([]model.Cluster, error) {
	if p.Limit <= 0 {
		return nil, fmt.Errorf("page limit must be positive, got %d", p.Limit)
	}
	rows, err := r.all(ctx, kube.TenantNamespace(tenantID), map[string]string{LabelTenant: tenantID})
	if err != nil {
		return nil, err
	}
	return page(rows, func(c model.Cluster) string { return c.ID }, p)
}

func (r clusters) ListByNetwork(ctx context.Context, networkID string) ([]model.Cluster, error) {
	return r.all(ctx, "", map[string]string{LabelNetworkID: networkID})
}

// ListByName returns the tenant's cluster of the name: at most one, the
// name is the object's.
func (r clusters) ListByName(ctx context.Context, tenantID, name string) ([]model.Cluster, error) {
	rows, err := r.all(ctx, kube.TenantNamespace(tenantID), map[string]string{LabelTenant: tenantID})
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, c := range rows {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out, nil
}

// Update writes the row if the stored etag is c's, and gives c a new one
// (networks.Update).
func (r clusters) Update(ctx context.Context, c *model.Cluster) error {
	old, updated := c.ETag, c.UpdatedAt
	var obj *cloudv1alpha1.Cluster
	err := retry(func() error {
		var err error
		if obj, err = r.find(ctx, c.TenantID, c.ID); err != nil {
			return err
		}
		m := metaOf(obj)
		if m.ETag != old {
			return repository.ErrConflict
		}
		c.ETag, c.UpdatedAt, c.CreatedAt = newETag(), now(), m.CreatedAt
		if err := r.fill(c, obj.Spec.Network.NetworkRef.Name, obj); err != nil {
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

// writeStatus is networks.writeStatus for a cluster.
func (r clusters) writeStatus(ctx context.Context, tenantID, id, etag string, status model.JSON, obj *cloudv1alpha1.Cluster) error {
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
func (r clusters) SetStatus(ctx context.Context, tenantID, id, etag string, status model.JSON) error {
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

// Delete removes the cluster.
func (r clusters) Delete(ctx context.Context, tenantID, id string) error {
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
	r.release(ctx, obj.Namespace, clusterKeys(row)...)
	return nil
}
