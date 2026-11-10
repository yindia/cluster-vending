package idl_test

import (
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func validBootstrap() *cloudv1.BootstrapTenantRequest {
	return &cloudv1.BootstrapTenantRequest{
		TenantId: "acme",
		Account: &cloudv1.CloudAccount{
			AccountName: "Acme production",
			Provider: &cloudv1.CloudAccount_Aws{Aws: &cloudv1.AwsAccount{
				AccountId: "444455556666", Role: "cluster-vending-provisioner",
			}},
		},
	}
}

func TestBootstrapTenantRequestValidation(t *testing.T) {
	type req = cloudv1.BootstrapTenantRequest
	tests := []struct {
		name    string
		mutate  func(r *req)
		wantErr bool
	}{
		{name: "valid", mutate: func(*req) {}},
		{name: "no account name", mutate: func(r *req) { r.Account.AccountName = "" }},
		{name: "bad tenant id", wantErr: true, mutate: func(r *req) { r.TenantId = "Acme_Corp" }},
		{name: "no tenant id", wantErr: true, mutate: func(r *req) { r.TenantId = "" }},
		{name: "no account", wantErr: true, mutate: func(r *req) { r.Account = nil }},
		{name: "no provider", wantErr: true, mutate: func(r *req) { r.Account.Provider = nil }},
		{name: "other role name", mutate: func(r *req) { r.Account.GetAws().Role = "acme-platform_admin" }},
		{name: "no role name", wantErr: true, mutate: func(r *req) { r.Account.GetAws().Role = "" }},
		{name: "role name with path", wantErr: true, mutate: func(r *req) { r.Account.GetAws().Role = "team/admin" }},
		{name: "role name too long", wantErr: true, mutate: func(r *req) { r.Account.GetAws().Role = strings.Repeat("r", 65) }},
		{name: "short aws account id", wantErr: true, mutate: func(r *req) { r.Account.GetAws().AccountId = "4444" }},
		{name: "empty aws account id", wantErr: true, mutate: func(r *req) { r.Account.GetAws().AccountId = "" }},
		{name: "aws account id not digits", wantErr: true, mutate: func(r *req) { r.Account.GetAws().AccountId = "acme-prod-1234" }},
		{name: "account name too long", wantErr: true, mutate: func(r *req) {
			r.Account.AccountName = strings.Repeat("a", 129)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := proto.CloneOf(validBootstrap())
			tt.mutate(r)
			err := protovalidate.Validate(r)
			if tt.wantErr && err == nil {
				t.Fatal("want a validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
