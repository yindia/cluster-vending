package database_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     database.Config
		wantErr string
	}{
		{name: "default", cfg: database.DefaultConfig()},
		{name: "postgres", cfg: database.Config{Driver: database.DriverPostgres, DSN: "postgres://u:p@db:5432/cv?sslmode=require"}},
		{name: "unknown driver", cfg: database.Config{Driver: "mysql", DSN: "x"}, wantErr: "driver"},
		{name: "empty dsn", cfg: database.Config{Driver: database.DriverSQLite}, wantErr: "dsn"},
		{name: "negative open conns", cfg: database.Config{Driver: database.DriverSQLite, DSN: "x.db", MaxOpenConns: -1}, wantErr: "max open"},
		{name: "negative idle conns", cfg: database.Config{Driver: database.DriverSQLite, DSN: "x.db", MaxIdleConns: -1}, wantErr: "max idle"},
		{name: "negative lifetime", cfg: database.Config{Driver: database.DriverSQLite, DSN: "x.db", ConnMaxLifetime: -time.Second}, wantErr: "lifetime"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want an error about %q", err, tt.wantErr)
			}
		})
	}
}

func TestOpenSQLiteMigratesAndEnforcesForeignKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cv.db")
	db, err := database.Open(t.Context(), database.Config{Driver: database.DriverSQLite, DSN: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close(db) })

	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatalf("second Migrate must be a no-op: %v", err)
	}
	for _, m := range model.All() {
		if !db.Migrator().HasTable(m) {
			t.Errorf("table for %T was not created", m)
		}
	}

	var fk int
	if err := db.Raw("PRAGMA foreign_keys").Scan(&fk).Error; err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("SQLite foreign keys are off; relationships would not be enforced")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file was not created: %v", err)
	}
}

func TestOpenKeepsCallerPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cv.db")
	db, err := database.Open(t.Context(), database.Config{
		Driver: database.DriverSQLite,
		DSN:    "file:" + path + "?_pragma=busy_timeout(1234)",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close(db) })

	var timeout, fk int
	db.Raw("PRAGMA busy_timeout").Scan(&timeout)
	db.Raw("PRAGMA foreign_keys").Scan(&fk)
	if timeout != 1234 || fk != 1 {
		t.Fatalf("busy_timeout = %d (want 1234), foreign_keys = %d (want 1)", timeout, fk)
	}
}

func TestOpenRejectsInvalidConfig(t *testing.T) {
	if _, err := database.Open(t.Context(), database.Config{Driver: "mysql", DSN: "x"}); err == nil {
		t.Fatal("Open accepted an unknown driver")
	}
}

func TestOpenPostgresUnreachable(t *testing.T) {
	_, err := database.Open(t.Context(), database.Config{
		Driver: database.DriverPostgres,
		DSN:    "host=127.0.0.1 port=1 user=cv dbname=cv sslmode=disable connect_timeout=2",
	})
	if err == nil {
		t.Fatal("Open succeeded against a closed port")
	}
}

// TestPostgres runs against a real server when CV_TEST_POSTGRES_DSN is set,
// e.g. postgres://postgres:postgres@localhost:5432/cv_test?sslmode=disable.
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("CV_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CV_TEST_POSTGRES_DSN not set")
	}
	db, err := database.Open(t.Context(), database.Config{Driver: database.DriverPostgres, DSN: dsn})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
}

func TestMigrateDropsOneAccountPerProviderIndex(t *testing.T) {
	cfg := database.Config{Driver: database.DriverSQLite, DSN: filepath.Join(t.TempDir(), "old.db"), MaxOpenConns: 1}
	db, err := database.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	// A database from before tenants had many accounts.
	if err := db.Exec("CREATE UNIQUE INDEX idx_cloud_accounts_tenant_provider ON cloud_accounts (tenant_id, provider)").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if db.Migrator().HasIndex(&model.CloudAccount{}, "idx_cloud_accounts_tenant_provider") {
		t.Error("the old unique index is still there")
	}
}

func TestMigrateBackfillsProviderConfigNames(t *testing.T) {
	cfg := database.Config{Driver: database.DriverSQLite, DSN: filepath.Join(t.TempDir(), "old.db"), MaxOpenConns: 1}
	db, err := database.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Tenant{ID: "acme"}).Error; err != nil {
		t.Fatal(err)
	}
	// A row written before the column existed.
	if err := db.Exec("INSERT INTO cloud_accounts (id, tenant_id, provider, account_id, created_at, updated_at) " +
		"VALUES ('0a1b2c3d-0000-4000-8000-000000000000', 'acme', 'aws', '444455556666', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var a model.CloudAccount
	if err := db.First(&a, "account_id = ?", "444455556666").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE tenants SET display_name = '' WHERE id = 'acme'").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var tenant model.Tenant
	if err := db.First(&tenant, "id = ?", "acme").Error; err != nil || tenant.DisplayName != "acme" {
		t.Errorf("tenant display name = %q, %v; want acme", tenant.DisplayName, err)
	}
	if a.ProviderConfigName != "aws-0a1b2c3d" || a.Namespace != "tenant-acme" {
		t.Errorf("backfilled name, namespace = %q, %q; want aws-0a1b2c3d, tenant-acme", a.ProviderConfigName, a.Namespace)
	}
}
