package subtenant

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/config"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// The API refuses a group outside naas.allowedGroups (PROP-05 6.2), naming the
// allowed patterns, on create and on update.
func TestAccessGroupsMustBeAllowed(t *testing.T) {
	policy := config.NaaS{AllowedGroups: []string{"cv-subtenant-*"}, DeniedGroups: []string{"cv-subtenant-platform"}}
	tests := []struct {
		name          string
		policy        config.NaaS
		admin, viewer []string
		ok            bool
	}{
		{name: "allowed by prefix", policy: policy, admin: []string{"cv-subtenant-acme-admin"}, viewer: []string{"cv-subtenant-acme-read"}, ok: true},
		{name: "no groups need no list", policy: config.NaaS{}, ok: true},
		{name: "outside the list", policy: policy, admin: []string{"cv-subtenant-acme-admin"}, viewer: []string{"payments-admins"}},
		{name: "denied first", policy: policy, admin: []string{"cv-subtenant-platform"}},
		{name: "empty list allows nothing", policy: config.NaaS{}, admin: []string{"cv-subtenant-acme-admin"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{})
			w.service = NewService(reconcile.Deps{Kube: w.kube, NaaS: tt.policy})
			req := createRequest()
			if tt.admin != nil || tt.viewer != nil {
				req.Subtenant.Spec.Access = &v1.SubTenantAccess{AdminGroups: tt.admin, ViewerGroups: tt.viewer}
			}
			created, err := w.service.CreateSubTenant(t.Context(), req)
			if tt.ok {
				if err != nil {
					t.Fatal(err)
				}
				// An update to a group outside the list is refused the same way.
				update := updateRequest(created.Subtenant, "spec.access")
				update.Subtenant.Spec = &v1.SubTenantSpec{Access: &v1.SubTenantAccess{ViewerGroups: []string{"outside"}}}
				_, err := w.service.UpdateSubTenant(t.Context(), update)
				requireCode(t, err, connect.CodeInvalidArgument)
				return
			}
			requireCode(t, err, connect.CodeInvalidArgument)
			if !strings.Contains(err.Error(), tt.policy.AllowedPatterns()) {
				t.Fatalf("the refusal does not name the allowed patterns: %v", err)
			}
		})
	}
}
