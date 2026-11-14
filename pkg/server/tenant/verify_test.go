package tenant

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"go.uber.org/mock/gomock"

	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestVerifyCloudAccount(t *testing.T) {
	const id, unknown = "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "0b1f6b2c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"
	tests := []struct {
		name string
		req  *cloudv1.VerifyCloudAccountRequest
		code connect.Code // 0: ok
	}{
		{name: "records the request", req: &cloudv1.VerifyCloudAccountRequest{TenantId: "acme", Id: id}},
		{name: "unknown account", req: &cloudv1.VerifyCloudAccountRequest{TenantId: "acme", Id: unknown}, code: connect.CodeNotFound},
		{name: "not a bootstrap ID", req: &cloudv1.VerifyCloudAccountRequest{TenantId: "acme", Id: account}, code: connect.CodeInvalidArgument},
		{name: "no tenant", req: &cloudv1.VerifyCloudAccountRequest{Id: id}, code: connect.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			row := stored(account)
			row.ID = id
			var asked time.Time
			f.tenants.EXPECT().RequestAccessCheck(gomock.Any(), "acme", id, gomock.Any()).DoAndReturn(
				func(_ context.Context, _, _ string, at time.Time) error { asked = at; return nil }).MaxTimes(1)
			f.tenants.EXPECT().RequestAccessCheck(gomock.Any(), "acme", unknown, gomock.Any()).Return(repository.ErrNotFound).MaxTimes(1)
			f.tenants.EXPECT().GetCloudAccountByID(gomock.Any(), "acme", id).Return(row, nil).MaxTimes(1)

			got, err := f.svc.VerifyCloudAccount(t.Context(), tt.req)

			if tt.code == 0 {
				if err != nil || asked.IsZero() || got.GetAccount().GetId() != id {
					t.Errorf("err %v, asked at %v, account %v", err, asked, got.GetAccount())
				}
				return
			}
			if connect.CodeOf(err) != tt.code {
				t.Errorf("err = %v, want %v", err, tt.code)
			}
		})
	}
}
