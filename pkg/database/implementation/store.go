// Package implementation implements pkg/database/repository with GORM, on
// SQLite and PostgreSQL alike. Open the database with database.Open, which
// enables the error translation these repositories rely on.
package implementation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// New returns a Store backed by db.
func New(db *gorm.DB) repository.Store {
	return store{db: db}
}

type store struct{ db *gorm.DB }

func (s store) Tenants() repository.Tenants     { return tenants(s) }
func (s store) Networks() repository.Networks   { return networks(s) }
func (s store) Clusters() repository.Clusters   { return clusters(s) }
func (s store) NodePools() repository.NodePools { return nodePools(s) }

func (s store) ManagedResources() repository.ManagedResources { return managedResources(s) }

func (s store) WithinTransaction(ctx context.Context, fn func(repository.Store) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(store{db: tx})
	})
}

// op tells translate what a foreign key violation means.
type op int

const (
	opWrite  op = iota // a referenced parent is missing
	opDelete           // other rows still reference this one
)

// translate maps GORM errors (translated by database.Open) to repository
// errors, keeping the cause.
func translate(err error, o op) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return repository.ErrNotFound
	case errors.Is(err, model.ErrInvalid):
		return fmt.Errorf("%w: %w", repository.ErrInvalid, err)
	case isCheckViolation(err):
		return fmt.Errorf("%w: %w", repository.ErrInvalid, err)
	case errors.Is(err, gorm.ErrDuplicatedKey):
		return fmt.Errorf("%w: %w", repository.ErrAlreadyExists, err)
	case isForeignKeyViolation(err) && o == opDelete:
		return fmt.Errorf("%w: %w", repository.ErrInUse, err)
	case isForeignKeyViolation(err):
		return fmt.Errorf("%w: referenced parent: %w", repository.ErrNotFound, err)
	default:
		return err
	}
}

// SQLite extended result codes for foreign key failures: 787 on insert and
// update, 1811 when an ON DELETE RESTRICT action fires. The GORM SQLite
// driver only translates 787, so both are checked here.
const (
	sqliteConstraintForeignKey = 787
	sqliteConstraintTrigger    = 1811
)

// isCheckViolation detects CHECK constraint failures: SQLite extended code
// 275, Postgres SQLSTATE 23514.
func isCheckViolation(err error) bool {
	var coded interface{ Code() int }
	if errors.As(err, &coded) && coded.Code() == 275 {
		return true
	}
	var pg interface{ SQLState() string }
	return errors.As(err, &pg) && pg.SQLState() == "23514"
}

func isForeignKeyViolation(err error) bool {
	if errors.Is(err, gorm.ErrForeignKeyViolated) {
		return true
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		c := coded.Code()
		return c == sqliteConstraintForeignKey || c == sqliteConstraintTrigger
	}
	return false
}

// resource is a model row with the shared resource columns.
type resource interface {
	Meta() *model.Resource
}

func create[T resource](ctx context.Context, db *gorm.DB, row T) error {
	m := row.Meta()
	m.ETag = newETag()
	if m.Generation == 0 {
		m.Generation = 1
	}
	return translate(db.WithContext(ctx).Create(row).Error, opWrite)
}

func first[T any](ctx context.Context, db *gorm.DB, where map[string]any) (*T, error) {
	var row T
	if err := db.WithContext(ctx).Where(where).Take(&row).Error; err != nil {
		return nil, translate(err, opWrite)
	}
	return &row, nil
}

// list returns a page of rows ordered by key ("name" or "id"); page.After
// is the key of the previous page's last row.
func list[T any](ctx context.Context, db *gorm.DB, where map[string]any, key string, page repository.Page) ([]T, error) {
	if page.Limit <= 0 {
		return nil, fmt.Errorf("page limit must be positive, got %d", page.Limit)
	}
	q := db.WithContext(ctx).Where(where).Order(key).Limit(page.Limit)
	if page.After != "" {
		q = q.Where(key+" > ?", page.After)
	}
	var rows []T
	if err := q.Find(&rows).Error; err != nil {
		return nil, translate(err, opWrite)
	}
	return rows, nil
}

// update writes every column of row if the stored etag is row's etag, and
// gives row a new etag. On failure row keeps its old etag.
func update[T resource](ctx context.Context, db *gorm.DB, row T) error {
	m := row.Meta()
	old := m.ETag
	m.ETag = newETag()
	res := db.WithContext(ctx).Model(row).
		Where("id = ? AND etag = ?", m.ID, old).
		Select("*").Omit("id", "created_at").
		Updates(row)
	if res.Error != nil {
		m.ETag = old
		return translate(res.Error, opWrite)
	}
	if res.RowsAffected == 1 {
		return nil
	}
	m.ETag = old
	var n int64
	if err := db.WithContext(ctx).Model(row).Where("id = ?", m.ID).Count(&n).Error; err != nil {
		return translate(err, opWrite)
	}
	if n == 0 {
		return repository.ErrNotFound
	}
	return repository.ErrConflict
}

// mustExist returns ErrNotFound unless a live (not deleted) row matches.
func mustExist[T any](ctx context.Context, db *gorm.DB, where map[string]any) error {
	_, err := first[T](ctx, db, where)
	return err
}

// notReferenced returns ErrInUse while a live row of child's table matches.
func notReferenced(ctx context.Context, db *gorm.DB, child any, where map[string]any) error {
	var n int64
	if err := db.WithContext(ctx).Model(child).Where(where).Count(&n).Error; err != nil {
		return translate(err, opWrite)
	}
	if n > 0 {
		return fmt.Errorf("%w: %d live %T rows reference it", repository.ErrInUse, n, child)
	}
	return nil
}

// remove soft-deletes the matching rows (every model has DeletedAt).
func remove[T any](ctx context.Context, db *gorm.DB, where map[string]any) error {
	res := db.WithContext(ctx).Where(where).Delete(new(T))
	if res.Error != nil {
		return translate(res.Error, opDelete)
	}
	if res.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func newETag() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}
