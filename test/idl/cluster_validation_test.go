// Package idl_test checks that the protovalidate rules declared in
// idl/cloud/v1 accept and reject the requests they are meant to.
package idl_test

import (
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func validCreate() *cloudv1.CreateClusterRequest {
	return &cloudv1.CreateClusterRequest{
		TenantId: "acme",
		Cluster: &cloudv1.Cluster{
			Name: "prod-us-east-1",
			Spec: &cloudv1.ClusterSpec{
				Region:  "us-east-1",
				Network: &cloudv1.ClusterNetwork{NetworkId: "0190f1a2-0000-7000-8000-000000000001"},
				ControlPlane: &cloudv1.ControlPlaneSpec{
					KubernetesVersion: "1.33",
					EndpointAccess: &cloudv1.EndpointAccess{
						PublicAccessCidrs: []string{"203.0.113.0/24"},
					},
				},
				DefaultNodePool: &cloudv1.NodePoolSpec{
					InstanceTypes: []string{"m6i.large"},
					ScalingConfig: &cloudv1.ScalingConfig{MinSize: 1, DesiredSize: proto.Uint32(2), MaxSize: 5},
				},
				Tags: map[string]string{"team": "platform"},
				Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{
					EnabledClusterLogTypes: []cloudv1.AwsClusterLogType{cloudv1.AwsClusterLogType_AWS_CLUSTER_LOG_TYPE_AUDIT},
				}},
			},
		},
	}
}

// mutate returns a modified deep copy of the valid request.
func mutate(f func(s *cloudv1.ClusterSpec, r *cloudv1.CreateClusterRequest)) *cloudv1.CreateClusterRequest {
	r := proto.Clone(validCreate()).(*cloudv1.CreateClusterRequest)
	f(r.Cluster.Spec, r)
	return r
}

// useNetwork points the cluster at a platform Network.
func useNetwork(s *cloudv1.ClusterSpec, id string) { s.Network.NetworkId = id }

func TestCreateClusterRequestValidation(t *testing.T) {
	type spec = cloudv1.ClusterSpec
	type req = cloudv1.CreateClusterRequest

	tests := []struct {
		name    string
		req     *cloudv1.CreateClusterRequest
		wantErr bool
	}{
		{name: "valid", req: validCreate()},
		{name: "valid multiple spot instance types", req: mutate(func(s *spec, _ *req) {
			s.DefaultNodePool.InstanceTypes = []string{"m6i.large", "m7i.large"}
			s.DefaultNodePool.CapacityType = cloudv1.CapacityType_CAPACITY_TYPE_SPOT
		})},

		{name: "network id not a uuid", wantErr: true, req: mutate(func(s *spec, _ *req) { useNetwork(s, "shared-use1") })},
		{name: "empty network id", wantErr: true, req: mutate(func(s *spec, _ *req) { useNetwork(s, "") })},

		// Zones of the network the cluster uses.
		{name: "zone ids", req: mutate(func(s *spec, _ *req) { s.Network.ZoneIds = []string{"use1-az1", "use1-az2"} })},
		{name: "aws: one zone id", wantErr: true, req: mutate(func(s *spec, _ *req) { s.Network.ZoneIds = []string{"use1-az1"} })},
		{name: "aws: zone name", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.Network.ZoneIds = []string{"us-east-1a", "us-east-1b"}
		})},
		{name: "duplicate zone ids", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.Network.ZoneIds = []string{"use1-az1", "use1-az1"}
		})},

		// Secrets encryption.
		{name: "secrets encryption", req: mutate(func(s *spec, _ *req) {
			s.ControlPlane.SecretsEncryption = &cloudv1.SecretsEncryption{Enabled: true}
		})},

		// Envelope.
		{name: "missing tenant", wantErr: true, req: mutate(func(_ *spec, r *req) { r.TenantId = "" })},
		{name: "tenant not dns label", wantErr: true, req: mutate(func(_ *spec, r *req) { r.TenantId = "Acme_Corp" })},
		{name: "service-linked role as admin", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.Access = &cloudv1.AccessSpec{AdminPrincipals: []string{
				"arn:aws:iam::312040893990:role/aws-service-role/eks.amazonaws.com/AWSServiceRoleForAmazonEKS"}}
		})},
		{name: "cluster name too long", wantErr: true, req: mutate(func(_ *spec, r *req) {
			r.Cluster.Name = "a123456789012345678901234567890123456789x"
		})},
		{name: "addons", req: mutate(func(s *spec, _ *req) {
			s.GetAws().Addons = []*cloudv1.AwsAddon{{Name: "aws-efs-csi-driver"}, {Name: "metrics-server", Version: "v0.8.0-eksbuild.1"},
				{Name: "aws-mountpoint-s3-csi-driver", S3BucketNames: []string{"team-data"}}}
		})},
		{name: "addon not supported", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.GetAws().Addons = []*cloudv1.AwsAddon{{Name: "adot"}}
		})},
		{name: "addon twice", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.GetAws().Addons = []*cloudv1.AwsAddon{{Name: "coredns"}, {Name: "coredns"}}
		})},
		{name: "mountpoint without buckets", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.GetAws().Addons = []*cloudv1.AwsAddon{{Name: "aws-mountpoint-s3-csi-driver"}}
		})},
		{name: "buckets on another addon", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.GetAws().Addons = []*cloudv1.AwsAddon{{Name: "coredns", S3BucketNames: []string{"team-data"}}}
		})},
		{name: "bad request id", wantErr: true, req: mutate(func(_ *spec, r *req) { r.RequestId = "abc" })},

		// Common contract.
		{name: "no provider", wantErr: true, req: mutate(func(s *spec, _ *req) { s.Provider = nil })},
		{name: "missing region", wantErr: true, req: mutate(func(s *spec, _ *req) { s.Region = "" })},
		{name: "bad public access cidr", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.ControlPlane.EndpointAccess.PublicAccessCidrs = []string{"not-a-cidr"}
		})},
		{name: "bad kubernetes version", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.ControlPlane.KubernetesVersion = "v1.33.1"
		})},
		{name: "no instance types", wantErr: true, req: mutate(func(s *spec, _ *req) { s.DefaultNodePool.InstanceTypes = nil })},
		{name: "desired above max", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.DefaultNodePool.ScalingConfig = &cloudv1.ScalingConfig{MinSize: 1, DesiredSize: proto.Uint32(9), MaxSize: 5}
		})},
		{name: "missing scaling config", wantErr: true, req: mutate(func(s *spec, _ *req) { s.DefaultNodePool.ScalingConfig = nil })},
		{name: "disk too small", wantErr: true, req: mutate(func(s *spec, _ *req) { s.DefaultNodePool.DiskSizeGb = 10 })},
		{name: "taint without effect", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.DefaultNodePool.Taints = []*cloudv1.Taint{{Key: "gpu"}}
		})},

		// AWS constraints on the common fields.
		{name: "aws: bad region", wantErr: true, req: mutate(func(s *spec, _ *req) { s.Region = "eastus" })},
		{name: "aws: bad instance type", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.DefaultNodePool.InstanceTypes = []string{"Standard_D4s_v5"}
		})},
		{name: "aws: reserved tag prefix", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.Tags = map[string]string{"AWS:foo": "bar"}
		})},
		{name: "aws: unspecified log type", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.GetAws().EnabledClusterLogTypes = []cloudv1.AwsClusterLogType{cloudv1.AwsClusterLogType_AWS_CLUSTER_LOG_TYPE_UNSPECIFIED}
		})},

		// Pod / service ranges.
		{name: "service cidr", req: mutate(func(s *spec, _ *req) { s.Network.ServiceCidr = "172.20.0.0/16" })},
		{name: "service cidr not a prefix", wantErr: true, req: mutate(func(s *spec, _ *req) { s.Network.ServiceCidr = "172.20.0.1/16" })},
		{name: "aws: service cidr too small", wantErr: true, req: mutate(func(s *spec, _ *req) { s.Network.ServiceCidr = "172.20.0.0/28" })},
		{name: "aws: pod cidr not allowed", wantErr: true, req: mutate(func(s *spec, _ *req) { s.Network.PodCidr = "100.64.0.0/14" })},

		// Tenant namespace "tenant-<id>" must fit 63 chars.
		{name: "tenant id 56 chars", req: mutate(func(_ *spec, r *req) { r.TenantId = strings.Repeat("a", 56) })},
		{name: "tenant id 57 chars", wantErr: true, req: mutate(func(_ *spec, r *req) { r.TenantId = strings.Repeat("a", 57) })},

		{name: "new cluster without network", wantErr: true, req: mutate(func(s *spec, _ *req) { s.Network = nil })},
		{name: "new cluster without node pool", wantErr: true, req: mutate(func(s *spec, _ *req) { s.DefaultNodePool = nil })},

		// Scaling: desired_size is create-only and optional.
		{name: "desired size omitted", req: mutate(func(s *spec, _ *req) {
			s.DefaultNodePool.ScalingConfig = &cloudv1.ScalingConfig{MinSize: 2, MaxSize: 5}
		})},
		{name: "min above max", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.DefaultNodePool.ScalingConfig = &cloudv1.ScalingConfig{MinSize: 6, MaxSize: 5}
		})},

		// Admin access.
		{name: "admin principals role and user", req: mutate(func(s *spec, _ *req) {
			s.Access = &cloudv1.AccessSpec{AdminPrincipals: []string{
				"arn:aws:iam::111122223333:role/platform/ops",
				"arn:aws:iam::111122223333:user/alice",
			}}
		})},
		{name: "aws: admin principal not an iam arn", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.Access = &cloudv1.AccessSpec{AdminPrincipals: []string{"group:platform@example.com"}}
		})},
		{name: "aws: admin principals with config map auth", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.Access = &cloudv1.AccessSpec{AdminPrincipals: []string{"arn:aws:iam::111122223333:role/ops"}}
			s.GetAws().AccessConfig = &cloudv1.AwsAccessConfig{
				AuthenticationMode: cloudv1.AwsAuthenticationMode_AWS_AUTHENTICATION_MODE_CONFIG_MAP,
			}
		})},
		{name: "duplicate admin principals", wantErr: true, req: mutate(func(s *spec, _ *req) {
			s.Access = &cloudv1.AccessSpec{AdminPrincipals: []string{
				"arn:aws:iam::111122223333:role/ops", "arn:aws:iam::111122223333:role/ops",
			}}
		})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := protovalidate.Validate(tt.req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestUpdateClusterRequestValidatesOnlyEnvelope(t *testing.T) {
	// A partial cluster carrying only the masked field must pass; the server
	// validates the merged result instead.
	partial := &cloudv1.UpdateClusterRequest{
		TenantId:   "acme",
		Cluster:    &cloudv1.Cluster{Id: "prod-us-east-1", DisplayName: "Prod"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}},
	}
	if err := protovalidate.Validate(partial); err != nil {
		t.Fatalf("partial update rejected: %v", err)
	}

	missing := proto.Clone(partial).(*cloudv1.UpdateClusterRequest)
	missing.Cluster = nil
	if err := protovalidate.Validate(missing); err == nil {
		t.Fatal("update without cluster accepted")
	}

	noMask := proto.Clone(partial).(*cloudv1.UpdateClusterRequest)
	noMask.UpdateMask = nil
	if err := protovalidate.Validate(noMask); err == nil {
		t.Fatal("update without update_mask accepted")
	}
}

func TestListClustersRequestValidation(t *testing.T) {
	if err := protovalidate.Validate(&cloudv1.ListClustersRequest{TenantId: "acme"}); err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
	if err := protovalidate.Validate(&cloudv1.ListClustersRequest{TenantId: "acme", PageSize: -1}); err == nil {
		t.Fatal("negative page_size accepted")
	}
}
