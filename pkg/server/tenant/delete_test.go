package tenant

import (
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"go.uber.org/mock/gomock"

	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestDeleteCloudAccount(t *testing.T) {
	const id, used, unknown = "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "1b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "0b1f6b2c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"
	tests := []struct {
		name, id, wantText string
		code               connect.Code // 0: ok
	}{
		{name: "deletes it", id: id},
		{name: "refused under its networks, naming them", id: used, code: connect.CodeFailedPrecondition, wantText: "prod"},
		{name: "unknown account", id: unknown, code: connect.CodeNotFound},
		{name: "not a bootstrap ID", id: "acme", code: connect.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.tenants.EXPECT().DeleteCloudAccount(gomock.Any(), "acme", id).Return(nil).MaxTimes(1)
			f.tenants.EXPECT().DeleteCloudAccount(gomock.Any(), "acme", used).
				Return(fmt.Errorf("%w: networks prod belong to the account; delete them first", repository.ErrInUse)).MaxTimes(1)
			f.tenants.EXPECT().DeleteCloudAccount(gomock.Any(), "acme", unknown).Return(repository.ErrNotFound).MaxTimes(1)

			_, err := f.svc.DeleteCloudAccount(t.Context(), &cloudv1.DeleteCloudAccountRequest{TenantId: "acme", Id: tt.id})

			if connect.CodeOf(err) != tt.code && !(tt.code == 0 && err == nil) {
				t.Fatalf("err = %v, want %v", err, tt.code)
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("err = %v, want it to name %q", err, tt.wantText)
			}
		})
	}
}
