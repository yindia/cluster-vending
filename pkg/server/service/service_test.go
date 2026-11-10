package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database"
	"github.com/yindia/cluster-vending/pkg/database/implementation"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

func TestToConnect(t *testing.T) {
	tests := []struct {
		err  error
		want connect.Code
	}{
		{fmt.Errorf("network: %w", repository.ErrNotFound), connect.CodeNotFound},
		{repository.ErrAlreadyExists, connect.CodeAlreadyExists},
		{repository.ErrConflict, connect.CodeAborted},
		{repository.ErrInUse, connect.CodeFailedPrecondition},
		{repository.ErrInvalid, connect.CodeInvalidArgument},
		{context.Canceled, connect.CodeCanceled},
		{context.DeadlineExceeded, connect.CodeDeadlineExceeded},
		{connect.NewError(connect.CodeUnavailable, "x"), connect.CodeUnavailable},
		{errors.New("pq: password=secret"), connect.CodeInternal},
	}
	for _, tt := range tests {
		if got := connect.CodeOf(ToConnect(tt.err)); got != tt.want {
			t.Errorf("ToConnect(%v) code = %v, want %v", tt.err, got, tt.want)
		}
	}
	if ToConnect(nil) != nil {
		t.Error("ToConnect(nil) != nil")
	}
	if msg := ToConnect(errors.New("pq: password=secret")).Error(); msg != "internal: internal error" {
		t.Errorf("internal error leaks details: %q", msg)
	}
}

func TestAWSTenantRoleARN(t *testing.T) {
	db, err := database.Open(t.Context(), database.Config{
		Driver: database.DriverSQLite, DSN: filepath.Join(t.TempDir(), "t.db"), MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	store := implementation.New(db)
	ctx := t.Context()
	if err := store.Tenants().Create(ctx, &model.Tenant{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	d := Deps{Store: store}

	if _, err := d.AWSTenantRoleARN(ctx, "acme", "0b1f6b2c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("without an account: %v, want NOT_FOUND", err)
	}
	for _, a := range []*model.CloudAccount{
		{TenantID: "acme", Provider: model.ProviderAWS, AccountID: "444455556666",
			Identity: "arn:aws:iam::444455556666:role/cluster-vending-provisioner"},
		{TenantID: "acme", Provider: model.ProviderGCP, AccountID: "acme-prod-1234"},
	} {
		if err := store.Tenants().PutCloudAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	gcp, err := store.Tenants().GetCloudAccount(ctx, "acme", model.ProviderGCP, "acme-prod-1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AWSTenantRoleARN(ctx, "acme", gcp.ID); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("gcp account: %v, want INVALID_ARGUMENT", err)
	}
	aws, err := store.Tenants().GetCloudAccount(ctx, "acme", model.ProviderAWS, "444455556666")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.AWSTenantRoleARN(ctx, "acme", aws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "arn:aws:iam::444455556666:role/cluster-vending-provisioner"; got != want {
		t.Errorf("role ARN = %s, want %s", got, want)
	}
}
