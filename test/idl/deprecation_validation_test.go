package idl_test

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestListClusterDeprecationsRequestValidation(t *testing.T) {
	valid := func() *cloudv1.ListClusterDeprecationsRequest {
		return &cloudv1.ListClusterDeprecationsRequest{TenantId: "acme", ClusterId: "0190f1a2-0000-7000-8000-000000000001"}
	}
	tests := []struct {
		name   string
		change func(*cloudv1.ListClusterDeprecationsRequest)
		wantOK bool
	}{
		{"the smallest request", func(*cloudv1.ListClusterDeprecationsRequest) {}, true},
		{"a page", func(r *cloudv1.ListClusterDeprecationsRequest) {
			r.PageSize, r.PageToken, r.OnlyRemoved = 50, "100", true
		}, true},
		{"no tenant", func(r *cloudv1.ListClusterDeprecationsRequest) { r.TenantId = "" }, false},
		{"a tenant that is no DNS label", func(r *cloudv1.ListClusterDeprecationsRequest) { r.TenantId = "Acme_1" }, false},
		{"a cluster name, not its id", func(r *cloudv1.ListClusterDeprecationsRequest) { r.ClusterId = "prod-eks" }, false},
		{"no cluster", func(r *cloudv1.ListClusterDeprecationsRequest) { r.ClusterId = "" }, false},
		{"a negative page size", func(r *cloudv1.ListClusterDeprecationsRequest) { r.PageSize = -1 }, false},
		{"a token that is no offset", func(r *cloudv1.ListClusterDeprecationsRequest) { r.PageToken = "next" }, false},
		{"a token that is too long", func(r *cloudv1.ListClusterDeprecationsRequest) { r.PageToken = "123456789" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := valid()
			tt.change(req)
			if err := protovalidate.Validate(req); (err == nil) != tt.wantOK {
				t.Fatalf("Validate = %v, want ok %v", err, tt.wantOK)
			}
		})
	}
}

// Reading what a scan found changes nothing.
func TestListClusterDeprecationsHasNoSideEffects(t *testing.T) {
	method := cloudv1.File_cloud_v1_cluster_service_proto.Services().ByName("ClusterService").Methods().ByName("ListClusterDeprecations")
	if method == nil {
		t.Fatal("ClusterService has no ListClusterDeprecations")
	}
	opts, ok := method.Options().(*descriptorpb.MethodOptions)
	if !ok || opts.GetIdempotencyLevel() != descriptorpb.MethodOptions_NO_SIDE_EFFECTS {
		t.Fatalf("idempotency level = %v, want NO_SIDE_EFFECTS", opts.GetIdempotencyLevel())
	}
	// The summary on a cluster's status is field 40, apart from the
	// fields other work adds next to the existing ones.
	field := (&cloudv1.ClusterStatus{}).ProtoReflect().Descriptor().Fields().ByName("deprecations")
	if field == nil || field.Number() != protoreflect.FieldNumber(40) {
		t.Fatalf("ClusterStatus.deprecations = %v, want field 40", field)
	}
}
