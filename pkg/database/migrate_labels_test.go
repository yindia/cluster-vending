package database_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/database"
	"github.com/yindia/cluster-vending/pkg/database/implementation"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

// Rows written before spec.tags became labels get their tags as labels
// (labels win), and spec.tags disappears from the stored spec and input.
func TestMigrateMovesSpecTagsToLabels(t *testing.T) {
	ctx := t.Context()
	db, err := database.Open(ctx, database.Config{Driver: database.DriverSQLite, DSN: filepath.Join(t.TempDir(), "cv.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := implementation.New(db)
	if err := store.Tenants().Create(ctx, &model.Tenant{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	account := &model.CloudAccount{TenantID: "acme", Provider: model.ProviderAWS, AccountID: "444455556666"}
	if err := store.Tenants().PutCloudAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	n := &model.Network{TenantID: "acme", Name: "prod", CloudAccountID: account.ID, Provider: model.ProviderAWS, Region: "us-east-1",
		Resource: model.Resource{State: "NETWORK_STATE_READY", Labels: model.Labels{"env": "prod"},
			Spec: model.JSON(`{"region":"us-east-1","tags":{"team":"platform","env":"old"},"aws":{}}`)},
		Input: model.JSON(`{"name":"prod","spec":{"region":"us-east-1","tags":{"cost":"42"}}}`)}
	if err := store.Networks().Create(ctx, n); err != nil {
		t.Fatal(err)
	}

	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	got, err := store.Networks().Get(ctx, "acme", n.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := model.Labels{"env": "prod", "team": "platform", "cost": "42"}
	if len(got.Labels) != len(want) {
		t.Fatalf("labels = %v, want %v", got.Labels, want)
	}
	for k, v := range want {
		if got.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, got.Labels[k], v)
		}
	}
	for name, raw := range map[string]model.JSON{"spec": got.Spec, "input": got.Input} {
		if strings.Contains(string(raw), `"tags"`) {
			t.Errorf("%s still has tags: %s", name, raw)
		}
		if !json.Valid(raw) {
			t.Errorf("%s is not JSON: %s", name, raw)
		}
	}
}
