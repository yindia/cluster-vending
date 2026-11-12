// Package database opens the cluster-vending database and migrates its
// schema. Locally it is an SQLite file (pure Go, no cgo); in production it
// is PostgreSQL. Tables are defined in pkg/database/model, data access in
// pkg/database/repository (interfaces) and pkg/database/implementation.
package database

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

// Supported drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// DefaultSQLiteFile is the local database file used by DefaultConfig.
const DefaultSQLiteFile = "cluster-vending.db"

var drivers = []string{DriverSQLite, DriverPostgres}

// Config selects and tunes the database.
type Config struct {
	// Driver is "sqlite" (local) or "postgres" (production).
	Driver string `yaml:"driver"`
	// DSN is the SQLite file path (or "file:" URI) or the PostgreSQL
	// connection string (URL or key=value form).
	DSN string `yaml:"dsn"`
	// Connection pool limits; zero means the database/sql default
	// (unlimited open connections, 2 idle, no lifetime).
	MaxOpenConns    int           `yaml:"maxOpenConns"`
	MaxIdleConns    int           `yaml:"maxIdleConns"`
	ConnMaxLifetime time.Duration `yaml:"connMaxLifetime"`
}

// DefaultConfig is a local SQLite file in the working directory.
func DefaultConfig() Config {
	return Config{Driver: DriverSQLite, DSN: DefaultSQLiteFile}
}

// Validate reports every problem in the configuration.
func (c Config) Validate() error {
	var errs []error
	if !slices.Contains(drivers, c.Driver) {
		errs = append(errs, fmt.Errorf("driver must be one of %v, got %q", drivers, c.Driver))
	}
	if c.DSN == "" {
		errs = append(errs, errors.New("dsn is required"))
	}
	if c.MaxOpenConns < 0 {
		errs = append(errs, fmt.Errorf("max open connections must not be negative, got %d", c.MaxOpenConns))
	}
	if c.MaxIdleConns < 0 {
		errs = append(errs, fmt.Errorf("max idle connections must not be negative, got %d", c.MaxIdleConns))
	}
	if c.ConnMaxLifetime < 0 {
		errs = append(errs, fmt.Errorf("connection max lifetime must not be negative, got %s", c.ConnMaxLifetime))
	}
	return errors.Join(errs...)
}

// Open connects to the database and checks the connection. Database errors
// are translated to gorm.ErrDuplicatedKey / gorm.ErrForeignKeyViolated so
// callers can tell them apart on both drivers.
func Open(ctx context.Context, cfg Config) (*gorm.DB, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("database config: %w", err)
	}
	db, err := gorm.Open(dialector(cfg), &gorm.Config{
		TranslateError: true,
		Logger:         logger.Discard,
		NowFunc:        func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("open %s database: %w", cfg.Driver, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("open %s database: %w", cfg.Driver, err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("connect to %s database: %w", cfg.Driver, err)
	}
	return db, nil
}

// Migrate creates or updates every table, index and foreign key. It is
// idempotent.
func Migrate(ctx context.Context, db *gorm.DB) error {
	if err := db.WithContext(ctx).AutoMigrate(model.All()...); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	// Indexes AutoMigrate does not drop.
	m := db.WithContext(ctx).Migrator()
	for _, old := range []struct {
		table any
		index string
	}{
		// One account per provider per tenant; a tenant now has many.
		{&model.CloudAccount{}, "idx_cloud_accounts_tenant_provider"},
		// Unique network and cluster names; both are now addressed by ID.
		{&model.Network{}, "idx_networks_tenant_name"},
		{&model.Cluster{}, "idx_clusters_tenant_name"},
	} {
		if m.HasIndex(old.table, old.index) {
			if err := m.DropIndex(old.table, old.index); err != nil {
				return fmt.Errorf("migrate database: drop index %s: %w", old.index, err)
			}
		}
	}
	if err := backfillClusterAccounts(ctx, db); err != nil {
		return err
	}
	// Accounts from before ProviderConfig names were stored: derive one from
	// the row ID (hex), "<provider>-<8 hex>" like model.CloudAccount.
	err := db.WithContext(ctx).Exec(
		"UPDATE cloud_accounts SET provider_config_name = provider || '-' || substr(replace(id, '-', ''), 1, 8) " +
			"WHERE provider_config_name = ''").Error
	if err != nil {
		return fmt.Errorf("migrate database: backfill provider config names: %w", err)
	}
	// Tenants created before display names defaulted to the ID.
	err = db.WithContext(ctx).Exec("UPDATE tenants SET display_name = id WHERE display_name = ''").Error
	if err != nil {
		return fmt.Errorf("migrate database: backfill tenant display names: %w", err)
	}
	// Their namespace is the tenant's; their identity is filled in by the
	// next bootstrap.
	err = db.WithContext(ctx).Exec(
		"UPDATE cloud_accounts SET namespace = 'tenant-' || tenant_id WHERE namespace = ''").Error
	if err != nil {
		return fmt.Errorf("migrate database: backfill namespaces: %w", err)
	}
	// Clusters from before the reconciler made the cloud lookups had them
	// made by the call that stored them. Before the next backfill: a row
	// not applied yet keeps its lookups to make.
	err = db.WithContext(ctx).Exec(
		"UPDATE clusters SET resolved_generation = generation WHERE resolved_generation = 0 AND applied_generation >= generation").Error
	if err != nil {
		return fmt.Errorf("migrate database: backfill clusters resolved generations: %w", err)
	}
	// Rows from before applied generations were recorded were applied.
	for _, table := range []string{"networks", "clusters"} {
		err := db.WithContext(ctx).Exec("UPDATE " + table + " SET applied_generation = generation WHERE applied_generation = 0").Error
		if err != nil {
			return fmt.Errorf("migrate database: backfill %s applied generations: %w", table, err)
		}
	}
	if err := migrateTagsToLabels(ctx, db); err != nil {
		return fmt.Errorf("migrate database: move spec.tags to labels: %w", err)
	}
	return nil
}

// Close closes the connection pool.
func Close(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func dialector(cfg Config) gorm.Dialector {
	if cfg.Driver == DriverPostgres {
		return postgres.Open(cfg.DSN)
	}
	return sqlite.Open(sqliteDSN(cfg.DSN))
}

// sqliteDSN turns on foreign keys (off by default in SQLite, and the
// relationships depend on them) and a busy timeout, keeping any pragma the
// caller set.
func sqliteDSN(dsn string) string {
	pragmas := []string{"foreign_keys(1)", "busy_timeout(5000)"}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	for _, p := range pragmas {
		name, _, _ := strings.Cut(p, "(")
		if strings.Contains(dsn, "_pragma="+name+"(") {
			continue
		}
		dsn += sep + "_pragma=" + p
		sep = "&"
	}
	return dsn
}

// backfillClusterAccounts gives clusters stored before
// clusters.cloud_account_id existed their network's account, so the unique
// index on (account, region, name) covers them too. Row by row: a cluster
// whose name another one of the account and region already has (allowed
// back then) keeps an empty account and stays outside the index.
func backfillClusterAccounts(ctx context.Context, db *gorm.DB) error {
	var rows []struct{ ID, CloudAccountID string }
	err := db.WithContext(ctx).Table("clusters").
		Select("clusters.id AS id, networks.cloud_account_id AS cloud_account_id").
		Joins("JOIN networks ON networks.id = clusters.network_id").
		Where("clusters.cloud_account_id = '' AND clusters.deleted_at IS NULL").
		Order("clusters.id").Scan(&rows).Error
	if err != nil {
		return fmt.Errorf("migrate database: clusters without an account: %w", err)
	}
	for _, r := range rows {
		err := db.WithContext(ctx).Model(&model.Cluster{}).Where("id = ?", r.ID).UpdateColumn("cloud_account_id", r.CloudAccountID).Error
		if err != nil && !errors.Is(err, gorm.ErrDuplicatedKey) {
			return fmt.Errorf("migrate database: account of cluster %s: %w", r.ID, err)
		}
	}
	return nil
}
