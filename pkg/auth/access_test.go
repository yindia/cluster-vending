package auth_test

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/yindia/cluster-vending/pkg/auth"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The audit log tells writes from reads by the RPC's idempotency level.
func TestWrites(t *testing.T) {
	method := func(service, name string) protoreflect.MethodDescriptor {
		t.Helper()
		var file protoreflect.FileDescriptor
		switch service {
		case "ClusterService":
			file = cloudv1.File_cloud_v1_cluster_service_proto
		case "AdminService":
			file = cloudv1.File_cloud_v1_admin_service_proto
		}
		m := file.Services().ByName(protoreflect.Name(service)).Methods().ByName(protoreflect.Name(name))
		if m == nil {
			t.Fatalf("%s.%s is not an RPC", service, name)
		}
		return m
	}
	for _, tt := range []struct {
		service, name string
		write         bool
	}{
		{"ClusterService", "CreateCluster", true},
		{"ClusterService", "DeleteCluster", true},
		{"ClusterService", "GetCluster", false},
		{"AdminService", "Freeze", true},
		{"AdminService", "ListReleases", false},
	} {
		if got := auth.Writes(method(tt.service, tt.name)); got != tt.write {
			t.Errorf("%s.%s writes = %v, want %v", tt.service, tt.name, got, tt.write)
		}
	}
}

func TestTenantOfARequest(t *testing.T) {
	if got := auth.Tenant((&cloudv1.GetClusterRequest{TenantId: "acme"}).ProtoReflect()); got != "acme" {
		t.Errorf("Tenant = %q, want acme", got)
	}
	if got := auth.Tenant((&cloudv1.GetComponentValuesRequest{Name: "x"}).ProtoReflect()); got != "" {
		t.Errorf("a request without tenant_id = %q", got)
	}
}
