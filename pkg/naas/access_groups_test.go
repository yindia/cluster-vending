package naas

import (
	"errors"
	"testing"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/config"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// openGroups allows every group: for tests of other behaviour than the allowlist.
var openGroups = config.NaaS{AllowedGroups: []string{"*"}}

func TestResolveAccessEnforcesTheGroupAllowlist(t *testing.T) {
	access := &cloud.SubTenantAccess{AdminGroups: []string{"cv-subtenant-admins"}, ViewerGroups: []string{"cv-subtenant-readers"}}
	tests := []struct {
		name    string
		groups  config.NaaS
		spec    *cloudv1.ClusterSpec
		allowed bool
	}{
		{"prefix allows both", config.NaaS{AllowedGroups: []string{"cv-subtenant-*"}}, accessCluster(), true},
		{"exact allows one only", config.NaaS{AllowedGroups: []string{"cv-subtenant-admins"}}, accessCluster(), false},
		{"deny is checked first", config.NaaS{AllowedGroups: []string{"cv-subtenant-*"}, DeniedGroups: []string{"cv-subtenant-readers"}}, accessCluster(), false},
		{"empty list closes access", config.NaaS{}, accessCluster(), false},
		{"refused before OIDC is looked at", config.NaaS{}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := ResolveAccess(access, tt.spec, tt.groups)
			if tt.allowed {
				if err != nil || len(resolved.Bindings) != 2 {
					t.Fatalf("ResolveAccess = %+v, %v", resolved, err)
				}
				return
			}
			if !errors.Is(err, ErrGroupNotAllowed) {
				t.Fatalf("err = %v, want ErrGroupNotAllowed", err)
			}
		})
	}
	// No group named needs no allowlist.
	if empty, err := ResolveAccess(&cloud.SubTenantAccess{}, nil, config.NaaS{}); err != nil || len(empty.Bindings) != 0 {
		t.Fatalf("empty intent = %+v, %v", empty, err)
	}
}
