package idl_test

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func crossplaneValues(v protoreflect.EnumValueDescriptor) *cloudv1.CrossplaneValues {
	return proto.GetExtension(v.Options(), cloudv1.E_Crossplane).(*cloudv1.CrossplaneValues)
}

// Every non-zero value of an enum that maps onto a Crossplane string field
// must declare the provider spelling, so the server never derives it from the
// proto name.
func TestEnumsDeclareCrossplaneValues(t *testing.T) {
	tests := []struct {
		enum     protoreflect.EnumDescriptor
		provider func(*cloudv1.CrossplaneValues) string
		name     string
	}{
		{name: "aws", enum: cloudv1.AwsAuthenticationMode(0).Descriptor(), provider: (*cloudv1.CrossplaneValues).GetAws},
		{name: "aws", enum: cloudv1.AwsClusterLogType(0).Descriptor(), provider: (*cloudv1.CrossplaneValues).GetAws},
		{name: "aws", enum: cloudv1.AwsSupportType(0).Descriptor(), provider: (*cloudv1.CrossplaneValues).GetAws},
		{name: "aws", enum: cloudv1.AwsAmiType(0).Descriptor(), provider: (*cloudv1.CrossplaneValues).GetAws},
		{name: "aws", enum: cloudv1.NatGatewayMode(0).Descriptor(), provider: (*cloudv1.CrossplaneValues).GetAws},
		{name: "aws", enum: cloudv1.CapacityType(0).Descriptor(), provider: (*cloudv1.CrossplaneValues).GetAws},
		{name: "aws", enum: cloudv1.TaintEffect(0).Descriptor(), provider: (*cloudv1.CrossplaneValues).GetAws},
		{name: "gcp", enum: cloudv1.TaintEffect(0).Descriptor(), provider: (*cloudv1.CrossplaneValues).GetGcp},
		{name: "azure", enum: cloudv1.TaintEffect(0).Descriptor(), provider: (*cloudv1.CrossplaneValues).GetAzure},
		{name: "azure", enum: cloudv1.CapacityType(0).Descriptor(), provider: (*cloudv1.CrossplaneValues).GetAzure},
	}

	for _, tt := range tests {
		t.Run(string(tt.enum.Name())+"/"+tt.name, func(t *testing.T) {
			values := tt.enum.Values()
			for i := range values.Len() {
				v := values.Get(i)
				if v.Number() == 0 {
					continue
				}
				if tt.provider(crossplaneValues(v)) == "" {
					t.Errorf("%s has no (crossplane).%s value", v.FullName(), tt.name)
				}
			}
		})
	}
}

// Spot-check spellings that cannot be derived from the proto names.
func TestCrossplaneValueSpellings(t *testing.T) {
	tests := []struct {
		value protoreflect.EnumValueDescriptor
		got   func(*cloudv1.CrossplaneValues) string
		want  string
	}{
		{cloudv1.AwsAmiType_AWS_AMI_TYPE_AL2023_X86_64_STANDARD.Descriptor().Values().ByNumber(1), (*cloudv1.CrossplaneValues).GetAws, "AL2023_x86_64_STANDARD"},
		{cloudv1.AwsClusterLogType(0).Descriptor().Values().ByNumber(4), (*cloudv1.CrossplaneValues).GetAws, "controllerManager"},
		{cloudv1.TaintEffect(0).Descriptor().Values().ByNumber(1), (*cloudv1.CrossplaneValues).GetAzure, "NoSchedule"},
		{cloudv1.NatGatewayMode(0).Descriptor().Values().ByNumber(3), (*cloudv1.CrossplaneValues).GetAws, "regional"},
	}
	for _, tt := range tests {
		t.Run(string(tt.value.FullName()), func(t *testing.T) {
			if got := tt.got(crossplaneValues(tt.value)); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
