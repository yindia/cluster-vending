package server_test

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/server"
)

func TestNewResultsStore(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDLOCAL")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")

	store, err := server.NewResultsStore(t.Context(), config.AWSAccess{SourceRoleARN: "arn:aws:iam::111122223333:role/src"})
	if err != nil || store == nil {
		t.Fatalf("NewResultsStore = %v, %v", store, err)
	}
	if _, err := server.NewResultsStore(t.Context(), config.AWSAccess{SourceRoleARN: "nope"}); err == nil {
		t.Error("a bad source role was accepted")
	}
}
