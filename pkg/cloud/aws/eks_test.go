package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/smithy-go"
)

// fakeEKS knows Kubernetes 1.33 (default) and 1.34, and vpc-cni
// v1.20.1 (default on 1.33), v1.20.0 (1.33 only) and v1.21.0 (1.34).
type fakeEKS struct{ describeErr error }

func (f fakeEKS) DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{}}, nil
}

func (fakeEKS) DescribeClusterVersions(context.Context, *eks.DescribeClusterVersionsInput, ...func(*eks.Options)) (*eks.DescribeClusterVersionsOutput, error) {
	return &eks.DescribeClusterVersionsOutput{ClusterVersions: []ekstypes.ClusterVersionInformation{
		{ClusterVersion: awssdk.String("1.33"), DefaultVersion: true},
	}}, nil
}

func (fakeEKS) DescribeAddonVersions(_ context.Context, in *eks.DescribeAddonVersionsInput, _ ...func(*eks.Options)) (*eks.DescribeAddonVersionsOutput, error) {
	if awssdk.ToString(in.AddonName) != "vpc-cni" {
		return &eks.DescribeAddonVersionsOutput{}, nil
	}
	compat := func(k8s string, def bool) []ekstypes.Compatibility {
		return []ekstypes.Compatibility{{ClusterVersion: awssdk.String(k8s), DefaultVersion: def}}
	}
	all := []ekstypes.AddonVersionInfo{
		{AddonVersion: awssdk.String("v1.21.0-eksbuild.1"), Compatibilities: compat("1.34", true)},
		{AddonVersion: awssdk.String("v1.20.1-eksbuild.1"), Compatibilities: compat("1.33", true)},
		{AddonVersion: awssdk.String("v1.20.0-eksbuild.1"), Compatibilities: compat("1.33", false)},
	}
	return &eks.DescribeAddonVersionsOutput{Addons: []ekstypes.AddonInfo{{AddonName: in.AddonName, AddonVersions: all}}}, nil
}

func TestEKSClusterExists(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		want    bool
		wantErr error
	}{
		{"exists", nil, true, nil},
		{"not found", &ekstypes.ResourceNotFoundException{}, false, nil},
		{"denied", &smithy.GenericAPIError{Code: "AccessDeniedException"}, false, ErrAccessDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant := &Tenant{AccountID: tenantAccount, Region: "us-east-1", eks: fakeEKS{describeErr: tt.err}}
			got, err := tenant.EKSClusterExists(context.Background(), "prod")
			if got != tt.want || !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Errorf("got %v, %v; want %v, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestResolveEKSVersions(t *testing.T) {
	tenant := &Tenant{AccountID: tenantAccount, Region: "us-east-1", eks: fakeEKS{}}
	tests := []struct {
		name        string
		req         VersionRequest
		wantK8s     string
		wantVersion string
		wantErr     bool
	}{
		{"defaults", VersionRequest{Addons: []AddonVersionRequest{{Name: "vpc-cni"}}}, "1.33", "v1.20.1-eksbuild.1", false},
		{"follows kubernetes", VersionRequest{KubernetesVersion: "1.34", Addons: []AddonVersionRequest{{Name: "vpc-cni"}}}, "1.34", "v1.21.0-eksbuild.1", false},
		{"pinned compatible", VersionRequest{KubernetesVersion: "1.33",
			Addons: []AddonVersionRequest{{Name: "vpc-cni", Version: "v1.20.0-eksbuild.1"}}}, "1.33", "v1.20.0-eksbuild.1", false},
		{"pinned incompatible", VersionRequest{KubernetesVersion: "1.34",
			Addons: []AddonVersionRequest{{Name: "vpc-cni", Version: "v1.20.0-eksbuild.1"}}}, "", "", true},
		{"unknown add-on", VersionRequest{Addons: []AddonVersionRequest{{Name: "nope"}}}, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tenant.ResolveEKSVersions(context.Background(), tt.req)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("err = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil || got.KubernetesVersion != tt.wantK8s || got.Addons[tt.req.Addons[0].Name].Version != tt.wantVersion {
				t.Errorf("got %+v, %v; want %s %s", got, err, tt.wantK8s, tt.wantVersion)
			}
		})
	}
}
