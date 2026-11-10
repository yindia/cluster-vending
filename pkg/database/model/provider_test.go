package model_test

import (
	"errors"
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

const (
	subscription = "0b1f6b2c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"
	directory    = "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
)

func TestValidateAccount(t *testing.T) {
	tests := []struct {
		name               string
		provider           model.Provider
		account, directory string
		ok                 bool
	}{
		{"aws", model.ProviderAWS, "444455556666", "", true},
		{"aws short", model.ProviderAWS, "4444", "", false},
		{"aws with directory", model.ProviderAWS, "444455556666", directory, false},
		{"gcp", model.ProviderGCP, "acme-prod-1234", "", true},
		{"gcp uppercase", model.ProviderGCP, "Acme-Prod", "", false},
		{"gcp trailing hyphen", model.ProviderGCP, "acme-prod-", "", false},
		{"gcp with directory", model.ProviderGCP, "acme-prod-1234", directory, false},
		{"azure", model.ProviderAzure, subscription, directory, true},
		{"azure no directory", model.ProviderAzure, subscription, "", false},
		{"azure bad subscription", model.ProviderAzure, "sub-1", directory, false},
		{"unknown provider", "openstack", "x", "", false},
		{"empty provider", "", "444455556666", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := model.ValidateAccount(tt.provider, tt.account, tt.directory)
			if tt.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.ok && !errors.Is(err, model.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestProviderValidate(t *testing.T) {
	for _, p := range []model.Provider{model.ProviderAWS, model.ProviderGCP, model.ProviderAzure} {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := model.Provider("AWS").Validate(); !errors.Is(err, model.ErrInvalid) {
		t.Errorf("AWS (uppercase): %v, want ErrInvalid", err)
	}
}
