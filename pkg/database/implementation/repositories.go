package implementation

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

type tenants struct{ db *gorm.DB }

func (r tenants) Create(ctx context.Context, t *model.Tenant) error {
	return translate(r.db.WithContext(ctx).Create(t).Error, opWrite)
}

func (r tenants) Get(ctx context.Context, id string) (*model.Tenant, error) {
	return first[model.Tenant](ctx, r.db, map[string]any{"id": id})
}

func (r tenants) Delete(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := mustExist[model.Tenant](ctx, tx, map[string]any{"id": id}); err != nil {
			return err
		}
		for _, child := range []any{&model.Network{}, &model.Cluster{}} {
			if err := notReferenced(ctx, tx, child, map[string]any{"tenant_id": id}); err != nil {
				return err
			}
		}
		if err := tx.Where("tenant_id = ?", id).Delete(&model.CloudAccount{}).Error; err != nil {
			return translate(err, opDelete)
		}
		return remove[model.Tenant](ctx, tx, map[string]any{"id": id})
	})
}

func (r tenants) PutCloudAccount(ctx context.Context, a *model.CloudAccount) error {
	if err := mustExist[model.Tenant](ctx, r.db, map[string]any{"id": a.TenantID}); err != nil {
		return err
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "provider"}, {Name: "account_id"}},
		// Matches the partial unique index (live rows only).
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "deleted_at IS NULL"}}},
		DoUpdates:   clause.AssignmentColumns([]string{"directory_id", "account_name", "namespace", "identity", "updated_at"}),
	}).Create(a).Error
	return translate(err, opWrite)
}

func (r tenants) GetCloudAccount(ctx context.Context, tenantID string, provider model.Provider, accountID string) (*model.CloudAccount, error) {
	return first[model.CloudAccount](ctx, r.db, map[string]any{"tenant_id": tenantID, "provider": provider, "account_id": accountID})
}

func (r tenants) GetCloudAccountByID(ctx context.Context, tenantID, id string) (*model.CloudAccount, error) {
	return first[model.CloudAccount](ctx, r.db, map[string]any{"tenant_id": tenantID, "id": id})
}

func (r tenants) ListCloudAccounts(ctx context.Context, tenantID string) ([]model.CloudAccount, error) {
	var rows []model.CloudAccount
	err := r.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("provider").Order("account_id").Find(&rows).Error
	return rows, translate(err, opWrite)
}

type networks struct{ db *gorm.DB }

func (r networks) Create(ctx context.Context, n *model.Network) error {
	if err := mustExist[model.Tenant](ctx, r.db, map[string]any{"id": n.TenantID}); err != nil {
		return err
	}
	a, err := first[model.CloudAccount](ctx, r.db, map[string]any{"id": n.CloudAccountID})
	if err != nil {
		return fmt.Errorf("cloud account: %w", err)
	}
	if a.TenantID != n.TenantID || a.Provider != n.Provider {
		return fmt.Errorf("%w: account %s belongs to tenant %s on %s, network to tenant %s on %s",
			repository.ErrInvalid, a.AccountID, a.TenantID, a.Provider, n.TenantID, n.Provider)
	}
	return create(ctx, r.db, n)
}

func (r networks) Update(ctx context.Context, n *model.Network) error { return update(ctx, r.db, n) }

func (r networks) SetStatus(ctx context.Context, tenantID, id, etag string, status model.JSON) error {
	return setStatus[model.Network](ctx, r.db, tenantID, id, etag, status)
}

func (r networks) Get(ctx context.Context, tenantID, id string) (*model.Network, error) {
	return first[model.Network](ctx, r.db, map[string]any{"tenant_id": tenantID, "id": id})
}

func (r networks) GetByCreateRequest(ctx context.Context, tenantID, requestID string) (*model.Network, error) {
	return first[model.Network](ctx, r.db, map[string]any{"tenant_id": tenantID, "create_request_id": requestID})
}

func (r networks) List(ctx context.Context, tenantID string, page repository.Page) ([]model.Network, error) {
	return list[model.Network](ctx, r.db, map[string]any{"tenant_id": tenantID}, "id", page)
}

func (r networks) Delete(ctx context.Context, tenantID, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		n, err := first[model.Network](ctx, tx, map[string]any{"tenant_id": tenantID, "id": id})
		if err != nil {
			return err
		}
		if err := notReferenced(ctx, tx, &model.Cluster{}, map[string]any{"network_id": n.ID}); err != nil {
			return err
		}
		return remove[model.Network](ctx, tx, map[string]any{"id": n.ID})
	})
}

type clusters struct{ db *gorm.DB }

// networkDeleting is the stored state of a network being deleted.
const networkDeleting = "NETWORK_STATE_DELETING"

// Create stores a cluster in its network. In one transaction it also
// claims the network: the network must not be DELETING, and its etag
// changes. DeleteNetwork marks the network DELETING with the etag it read
// before it checked for clusters, so of a create and a delete that race
// exactly one wins: the create fails with ErrConflict when the network is
// DELETING, the delete when a cluster was stored since it read the
// network.
func (r clusters) Create(ctx context.Context, c *model.Cluster) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		n, err := first[model.Network](ctx, tx, map[string]any{"id": c.NetworkID})
		if err != nil {
			return fmt.Errorf("network: %w", err)
		}
		if n.TenantID != c.TenantID || n.Provider != c.Provider {
			return fmt.Errorf("%w: network %s belongs to tenant %s on %s, cluster to tenant %s on %s",
				repository.ErrInvalid, n.Name, n.TenantID, n.Provider, c.TenantID, c.Provider)
		}
		// Not conditional on the etag read above: two creates in one network,
		// or the loop touching the network meanwhile, must not fail each other.
		claim := tx.Model(&model.Network{}).Where("id = ? AND state <> ?", n.ID, networkDeleting).UpdateColumn("etag", newETag())
		if claim.Error != nil {
			return translate(claim.Error, opWrite)
		}
		if claim.RowsAffected != 1 {
			return fmt.Errorf("%w: network %s is being deleted", repository.ErrConflict, n.ID)
		}
		// The network's account is the cluster's: with it the unique index on
		// (account, region, name) refuses a second cluster of the name.
		c.CloudAccountID = n.CloudAccountID
		return create(ctx, tx, c)
	})
}

func (r clusters) Update(ctx context.Context, c *model.Cluster) error { return update(ctx, r.db, c) }

func (r clusters) SetStatus(ctx context.Context, tenantID, id, etag string, status model.JSON) error {
	return setStatus[model.Cluster](ctx, r.db, tenantID, id, etag, status)
}

// setStatus writes the status column only (no etag, no update time), and
// only while the row has etag.
func setStatus[T any](ctx context.Context, db *gorm.DB, tenantID, id, etag string, status model.JSON) error {
	res := db.WithContext(ctx).Model(new(T)).Where("tenant_id = ? AND id = ? AND etag = ?", tenantID, id, etag).
		UpdateColumn("status", status)
	if res.Error != nil {
		return translate(res.Error, opWrite)
	}
	if res.RowsAffected == 1 {
		return nil
	}
	var n int64
	if err := db.WithContext(ctx).Model(new(T)).Where("tenant_id = ? AND id = ?", tenantID, id).Count(&n).Error; err != nil {
		return translate(err, opWrite)
	}
	if n == 0 {
		return repository.ErrNotFound
	}
	return repository.ErrConflict
}

func (r clusters) Get(ctx context.Context, tenantID, id string) (*model.Cluster, error) {
	return first[model.Cluster](ctx, r.db, map[string]any{"tenant_id": tenantID, "id": id})
}

func (r clusters) GetByCreateRequest(ctx context.Context, tenantID, requestID string) (*model.Cluster, error) {
	return first[model.Cluster](ctx, r.db, map[string]any{"tenant_id": tenantID, "create_request_id": requestID})
}

func (r clusters) List(ctx context.Context, tenantID string, page repository.Page) ([]model.Cluster, error) {
	return list[model.Cluster](ctx, r.db, map[string]any{"tenant_id": tenantID}, "id", page)
}

func (r clusters) ListByNetwork(ctx context.Context, networkID string) ([]model.Cluster, error) {
	var rows []model.Cluster
	err := r.db.WithContext(ctx).Where("network_id = ?", networkID).Order("id").Find(&rows).Error
	return rows, translate(err, opWrite)
}

func (r clusters) ListByName(ctx context.Context, tenantID, name string) ([]model.Cluster, error) {
	var rows []model.Cluster
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND name = ?", tenantID, name).Order("id").Find(&rows).Error
	return rows, translate(err, opWrite)
}

func (r clusters) Delete(ctx context.Context, tenantID, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		c, err := first[model.Cluster](ctx, tx, map[string]any{"tenant_id": tenantID, "id": id})
		if err != nil {
			return err
		}
		if err := tx.Where("cluster_id = ?", c.ID).Delete(&model.NodePool{}).Error; err != nil {
			return translate(err, opDelete)
		}
		return remove[model.Cluster](ctx, tx, map[string]any{"id": c.ID})
	})
}

type nodePools struct{ db *gorm.DB }

func (r nodePools) Create(ctx context.Context, p *model.NodePool) error {
	if err := mustExist[model.Cluster](ctx, r.db, map[string]any{"id": p.ClusterID}); err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	return create(ctx, r.db, p)
}

func (r nodePools) Update(ctx context.Context, p *model.NodePool) error { return update(ctx, r.db, p) }

func (r nodePools) Get(ctx context.Context, clusterID, name string) (*model.NodePool, error) {
	return first[model.NodePool](ctx, r.db, map[string]any{"cluster_id": clusterID, "name": name})
}

func (r nodePools) List(ctx context.Context, clusterID string, page repository.Page) ([]model.NodePool, error) {
	return list[model.NodePool](ctx, r.db, map[string]any{"cluster_id": clusterID}, "name", page)
}

func (r nodePools) Delete(ctx context.Context, clusterID, name string) error {
	return remove[model.NodePool](ctx, r.db, map[string]any{"cluster_id": clusterID, "name": name})
}

type managedResources struct{ db *gorm.DB }

func (r managedResources) Put(ctx context.Context, objects []model.ManagedResource) error {
	if len(objects) == 0 {
		return nil
	}
	// Copy: Create writes generated IDs back, and the caller's slice must
	// stay reusable.
	rows := make([]model.ManagedResource, len(objects))
	for i, o := range objects {
		o.Base = model.Base{}
		rows[i] = o
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "owner_kind"}, {Name: "owner_id"}, {Name: "api_version"},
			{Name: "kind"}, {Name: "namespace"}, {Name: "name"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "deleted_at IS NULL"}}},
		DoNothing:   true,
	}).Create(&rows).Error
	return translate(err, opWrite)
}

func (r managedResources) ListByOwner(ctx context.Context, kind model.OwnerKind, ownerID string) ([]model.ManagedResource, error) {
	var rows []model.ManagedResource
	err := r.db.WithContext(ctx).Where("owner_kind = ? AND owner_id = ?", kind, ownerID).Order("id").Find(&rows).Error
	return rows, translate(err, opWrite)
}

func (r managedResources) ListByAccount(ctx context.Context, tenantID, cloudAccountID string) ([]model.ManagedResource, error) {
	var rows []model.ManagedResource
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND cloud_account_id = ?", tenantID, cloudAccountID).
		Order("id").Find(&rows).Error
	return rows, translate(err, opWrite)
}

func (r managedResources) DeleteByOwner(ctx context.Context, kind model.OwnerKind, ownerID string) error {
	err := r.db.WithContext(ctx).Where("owner_kind = ? AND owner_id = ?", kind, ownerID).
		Delete(&model.ManagedResource{}).Error
	return translate(err, opDelete)
}

func (r managedResources) Delete(ctx context.Context, id string) error {
	return remove[model.ManagedResource](ctx, r.db, map[string]any{"id": id})
}

// pending is the background loop's filter (see repository.Networks.ListPending).
func pending(ctx context.Context, db *gorm.DB, states []string, renderTarget int64) *gorm.DB {
	return db.WithContext(ctx).
		Where("state IN ? OR applied_generation < generation OR render_version < ?", states, renderTarget).Order("id")
}

func (r networks) ListPending(ctx context.Context, states []string, renderTarget int64) ([]model.Network, error) {
	var rows []model.Network
	err := pending(ctx, r.db, states, renderTarget).Find(&rows).Error
	return rows, translate(err, opWrite)
}

func (r clusters) ListPending(ctx context.Context, states []string, renderTarget int64) ([]model.Cluster, error) {
	var rows []model.Cluster
	err := pending(ctx, r.db, states, renderTarget).Find(&rows).Error
	return rows, translate(err, opWrite)
}

func (r clusters) CountByState(ctx context.Context) (map[string]int64, error) {
	var rows []struct {
		State string
		N     int64
	}
	err := r.db.WithContext(ctx).Model(&model.Cluster{}).Select("state, COUNT(*) AS n").Group("state").Scan(&rows).Error
	if err != nil {
		return nil, translate(err, opWrite)
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.State] = row.N
	}
	return out, nil
}

func (r managedResources) ListByOwners(ctx context.Context, kind model.OwnerKind, ownerIDs []string) (map[string][]model.ManagedResource, error) {
	out := make(map[string][]model.ManagedResource, len(ownerIDs))
	if len(ownerIDs) == 0 {
		return out, nil
	}
	var rows []model.ManagedResource
	err := r.db.WithContext(ctx).Where("owner_kind = ? AND owner_id IN ?", kind, ownerIDs).Order("id").Find(&rows).Error
	if err != nil {
		return nil, translate(err, opWrite)
	}
	for _, row := range rows {
		out[row.OwnerID] = append(out[row.OwnerID], row)
	}
	return out, nil
}

func (r managedResources) SetExternalName(ctx context.Context, id, externalName string) error {
	err := r.db.WithContext(ctx).Model(&model.ManagedResource{}).Where("id = ?", id).
		Update("external_name", externalName).Error
	return translate(err, opWrite)
}

func (r managedResources) SetGoneAt(ctx context.Context, id string, at time.Time) error {
	err := r.db.WithContext(ctx).Model(&model.ManagedResource{}).Where("id = ? AND gone_at IS NULL", id).
		Update("gone_at", at).Error
	return translate(err, opWrite)
}
