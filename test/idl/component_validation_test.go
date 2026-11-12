package idl_test

import (
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	componentClusterID = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"
	componentID        = "0b5e2a8c-1d4f-4e6a-9c3b-7f2d8e1a4b6c"
)

// validate runs protovalidate on msg and fails the test unless the outcome
// is the one wanted.
func validate(t *testing.T, msg proto.Message, wantErr bool) {
	t.Helper()
	err := protovalidate.Validate(msg)
	if wantErr && err == nil {
		t.Fatal("want a validation error")
	}
	if !wantErr && err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func validEnable(t *testing.T) *cloudv1.EnableComponentRequest {
	t.Helper()
	values, err := structpb.NewStruct(map[string]any{"replicas": 2})
	if err != nil {
		t.Fatal(err)
	}
	return &cloudv1.EnableComponentRequest{
		TenantId: "acme",
		Component: &cloudv1.Component{
			ClusterId: componentClusterID, Component: "karpenter", Values: values,
		},
	}
}

func TestEnableComponentRequestValidation(t *testing.T) {
	type req = cloudv1.EnableComponentRequest
	tests := []struct {
		name    string
		mutate  func(r *req)
		wantErr bool
	}{
		{name: "valid", mutate: func(*req) {}},
		{name: "no values", mutate: func(r *req) { r.Component.Values = nil }},
		{name: "pinned version", mutate: func(r *req) { r.Component.Version = "1.8.1" }},
		{name: "request id", mutate: func(r *req) { r.RequestId = componentID }},
		{name: "validate only", mutate: func(r *req) { r.ValidateOnly = true }},
		{name: "output-only fields are ignored", mutate: func(r *req) {
			r.Component.Id, r.Component.Etag, r.Component.Generation = "x", "y", 3
		}},
		{name: "bad tenant id", wantErr: true, mutate: func(r *req) { r.TenantId = "Acme_Corp" }},
		{name: "no component", wantErr: true, mutate: func(r *req) { r.Component = nil }},
		{name: "no cluster id", wantErr: true, mutate: func(r *req) { r.Component.ClusterId = "" }},
		{name: "cluster id not a uuid", wantErr: true, mutate: func(r *req) { r.Component.ClusterId = "prod-eks" }},
		{name: "no component name", wantErr: true, mutate: func(r *req) { r.Component.Component = "" }},
		{name: "component name not a DNS label", wantErr: true, mutate: func(r *req) { r.Component.Component = "Karpenter" }},
		{name: "component name too long", wantErr: true, mutate: func(r *req) { r.Component.Component = strings.Repeat("k", 64) }},
		{name: "version too long", wantErr: true, mutate: func(r *req) { r.Component.Version = strings.Repeat("1", 65) }},
		{name: "request id not a uuid", wantErr: true, mutate: func(r *req) { r.RequestId = "retry-1" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := proto.CloneOf(validEnable(t))
			tt.mutate(r)
			validate(t, r, tt.wantErr)
		})
	}
}

func TestUpdateComponentRequestValidation(t *testing.T) {
	type req = cloudv1.UpdateComponentRequest
	valid := func() *req {
		return &req{
			TenantId:   "acme",
			Component:  &cloudv1.Component{Id: componentID, Version: "1.8.1"},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"version"}},
		}
	}
	tests := []struct {
		name    string
		mutate  func(r *req)
		wantErr bool
	}{
		{name: "valid", mutate: func(*req) {}},
		{name: "values and version", mutate: func(r *req) { r.UpdateMask.Paths = []string{"values", "version"} }},
		{name: "values cleared", mutate: func(r *req) { r.UpdateMask.Paths = []string{"values"} }},
		{name: "no component", wantErr: true, mutate: func(r *req) { r.Component = nil }},
		{name: "no component id", wantErr: true, mutate: func(r *req) { r.Component.Id = "" }},
		{name: "no mask", wantErr: true, mutate: func(r *req) { r.UpdateMask = nil }},
		{name: "empty mask", wantErr: true, mutate: func(r *req) { r.UpdateMask.Paths = nil }},
		{name: "immutable path", wantErr: true, mutate: func(r *req) { r.UpdateMask.Paths = []string{"cluster_id"} }},
		{name: "component path", wantErr: true, mutate: func(r *req) { r.UpdateMask.Paths = []string{"values", "component"} }},
		{name: "bad tenant id", wantErr: true, mutate: func(r *req) { r.TenantId = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid()
			tt.mutate(r)
			validate(t, r, tt.wantErr)
		})
	}
}

func TestComponentByIDRequestsValidation(t *testing.T) {
	tests := []struct {
		name    string
		msg     proto.Message
		wantErr bool
	}{
		{name: "disable", msg: &cloudv1.DisableComponentRequest{TenantId: "acme", ComponentId: componentID, Force: true}},
		{name: "disable without id", wantErr: true, msg: &cloudv1.DisableComponentRequest{TenantId: "acme"}},
		{name: "disable bad tenant", wantErr: true, msg: &cloudv1.DisableComponentRequest{TenantId: "-acme", ComponentId: componentID}},
		{name: "get", msg: &cloudv1.GetComponentRequest{TenantId: "acme", ComponentId: componentID}},
		{name: "get id not a uuid", wantErr: true, msg: &cloudv1.GetComponentRequest{TenantId: "acme", ComponentId: "karpenter"}},
		{name: "list", msg: &cloudv1.ListComponentsRequest{TenantId: "acme"}},
		{name: "list of a cluster", msg: &cloudv1.ListComponentsRequest{TenantId: "acme", ClusterId: componentClusterID, PageSize: 10}},
		{name: "list cluster not a uuid", wantErr: true, msg: &cloudv1.ListComponentsRequest{TenantId: "acme", ClusterId: "prod"}},
		{name: "list negative page size", wantErr: true, msg: &cloudv1.ListComponentsRequest{TenantId: "acme", PageSize: -1}},
		{name: "catalog", msg: &cloudv1.ListComponentDefinitionsRequest{TenantId: "acme"}},
		{name: "catalog of a cluster", msg: &cloudv1.ListComponentDefinitionsRequest{TenantId: "acme", ClusterId: componentClusterID}},
		{name: "catalog cluster not a uuid", wantErr: true, msg: &cloudv1.ListComponentDefinitionsRequest{TenantId: "acme", ClusterId: "prod"}},
		{name: "catalog without tenant", wantErr: true, msg: &cloudv1.ListComponentDefinitionsRequest{}},
		{name: "values file", msg: &cloudv1.GetComponentValuesRequest{Name: "karpenter"}},
		{name: "values file without name", wantErr: true, msg: &cloudv1.GetComponentValuesRequest{}},
		{name: "values file name not a DNS label", wantErr: true, msg: &cloudv1.GetComponentValuesRequest{Name: "Karpenter"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validate(t, tt.msg, tt.wantErr)
		})
	}
}

// The read RPCs are marked free of side effects, so Connect clients may
// retry them and use GET; the mutations are not.
func TestComponentServiceIdempotency(t *testing.T) {
	service := cloudv1.File_cloud_v1_component_service_proto.Services().ByName("ComponentService")
	if service == nil {
		t.Fatal("no ComponentService")
	}
	want := map[protoreflect.Name]bool{
		"ListComponentDefinitions": true, "GetComponentValues": true, "GetComponent": true, "ListComponents": true,
		"EnableComponent": false, "UpdateComponent": false, "DisableComponent": false,
	}
	methods := service.Methods()
	if methods.Len() != len(want) {
		t.Errorf("%d methods, want %d", methods.Len(), len(want))
	}
	for name, readOnly := range want {
		m := methods.ByName(name)
		if m == nil {
			t.Errorf("no %s", name)
			continue
		}
		opts, ok := m.Options().(*descriptorpb.MethodOptions)
		level := opts.GetIdempotencyLevel()
		if !ok || (level == descriptorpb.MethodOptions_NO_SIDE_EFFECTS) != readOnly {
			t.Errorf("%s: idempotency level %v, want NO_SIDE_EFFECTS = %v", name, level, readOnly)
		}
	}
}
