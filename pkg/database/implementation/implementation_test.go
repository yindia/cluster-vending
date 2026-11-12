package implementation_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yindia/cluster-vending/pkg/database"
	"github.com/yindia/cluster-vending/pkg/database/implementation"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
)

// newStore opens a fresh, migrated database for one test: SQLite by
// default, PostgreSQL when CV_TEST_POSTGRES_DSN is set (each test then gets
// its own schema, dropped afterwards), e.g.
//
//	CV_TEST_POSTGRES_DSN='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
func newStore(t *testing.T) repository.Store {
	t.Helper()
	s, _ := newStoreDB(t)
	return s
}

// newStoreDB also returns the database, for checks that look past soft
// deletes (Unscoped).
func newStoreDB(t *testing.T) (repository.Store, *gorm.DB) {
	t.Helper()
	cfg := database.Config{Driver: database.DriverSQLite, DSN: filepath.Join(t.TempDir(), "cv.db")}
	if dsn := os.Getenv("CV_TEST_POSTGRES_DSN"); dsn != "" {
		cfg = database.Config{Driver: database.DriverPostgres, DSN: postgresSchema(t, dsn)}
	}
	db, err := database.Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return implementation.New(db), db
}

// postgresSchema creates a schema for one test and returns a DSN using it.
func postgresSchema(t *testing.T, dsn string) string {
	t.Helper()
	admin, err := database.Open(t.Context(), database.Config{Driver: database.DriverPostgres, DSN: dsn})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = database.Close(admin)
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}

// The behaviour every store has (pkg/database/storetest), on the database.
func TestStore(t *testing.T) { storetest.Run(t, newStore) }

// Deleting is a soft delete here: the row stays, marked deleted.
func TestDeleteIsSoft(t *testing.T) {
	s, db := newStoreDB(t)
	storetest.MustTenant(t, s, "acme")
	first := storetest.MustNetwork(t, s, "acme", "prod")

	if err := s.Networks().Delete(t.Context(), "acme", first.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var kept model.Network
	if err := db.Unscoped().Where("id = ?", first.ID).Take(&kept).Error; err != nil {
		t.Fatalf("deleted row is gone: %v", err)
	}
	if !kept.DeletedAt.Valid {
		t.Fatal("deleted row has no deleted_at")
	}
}

// Clusters stored before they recorded their account get it on the next
// migration, so the name index covers them.
func TestMigrateGivesOldClustersTheirAccount(t *testing.T) {
	s, db := newStoreDB(t)
	ctx := t.Context()
	storetest.MustTenant(t, s, "acme")
	n := storetest.MustNetwork(t, s, "acme", "n")
	c := storetest.MustCluster(t, s, "acme", "prod", n)
	if err := db.Exec("UPDATE clusters SET cloud_account_id = '' WHERE id = ?", c.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got, err := s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil || got.CloudAccountID != n.CloudAccountID {
		t.Fatalf("account after the migration = %q (%v), want the network's %q", got.CloudAccountID, err, n.CloudAccountID)
	}
	again := &model.Cluster{TenantID: "acme", Name: "prod", NetworkID: n.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: "CLUSTER_STATE_PROVISIONING", Spec: storetest.Spec("cluster")}}
	storetest.RequireErr(t, s.Clusters().Create(ctx, again), repository.ErrAlreadyExists)
}
