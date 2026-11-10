package idl_test

// TEMPORARY: tests for ClusterSpec.existing_cluster (cluster_import.proto).
// Delete this file together with that field.

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// validImport imports the Terraform-built EKS cluster "tf-prod" (and its
// node group "tf-default") running in the imported Network "tf-prod".
func validImport() *cloudv1.CreateClusterRequest {
	return mutate(func(s *cloudv1.ClusterSpec, r *cloudv1.CreateClusterRequest) {
		r.Cluster.Id = "tf-prod"
		useNetwork(s, "0190f1a2-0000-7000-8000-0000000000f1") // the imported network's id
		s.ExistingCluster = &cloudv1.ExistingCluster{Name: "tf-prod", DefaultNodePoolName: "tf-default"}
	})
}

func TestExistingClusterValidation(t *testing.T) {
	with := func(f func(e *cloudv1.ExistingCluster, s *cloudv1.ClusterSpec)) *cloudv1.CreateClusterRequest {
		r := proto.Clone(validImport()).(*cloudv1.CreateClusterRequest)
		f(r.Cluster.Spec.ExistingCluster, r.Cluster.Spec)
		return r
	}
	tests := []struct {
		name    string
		req     *cloudv1.CreateClusterRequest
		wantErr bool
	}{
		{name: "import, observe only", req: validImport()},
		{name: "import, new node pool", req: with(func(e *cloudv1.ExistingCluster, _ *cloudv1.ClusterSpec) { e.DefaultNodePoolName = "" })},
		{name: "manage-no-delete", req: with(func(e *cloudv1.ExistingCluster, _ *cloudv1.ClusterSpec) {
			e.Policy = cloudv1.ClusterImportPolicy_CLUSTER_IMPORT_POLICY_MANAGE_NO_DELETE
		})},
		{name: "full with acknowledgement", req: with(func(e *cloudv1.ExistingCluster, _ *cloudv1.ClusterSpec) {
			e.Policy = cloudv1.ClusterImportPolicy_CLUSTER_IMPORT_POLICY_FULL
			e.AcknowledgeNotImported = true
		})},

		{name: "full without acknowledgement", wantErr: true, req: with(func(e *cloudv1.ExistingCluster, _ *cloudv1.ClusterSpec) {
			e.Policy = cloudv1.ClusterImportPolicy_CLUSTER_IMPORT_POLICY_FULL
		})},
		{name: "bad cluster name", wantErr: true, req: with(func(e *cloudv1.ExistingCluster, _ *cloudv1.ClusterSpec) { e.Name = "-bad name" })},
		{name: "missing name", wantErr: true, req: with(func(e *cloudv1.ExistingCluster, _ *cloudv1.ClusterSpec) { e.Name = "" })},
		{name: "unknown policy", wantErr: true, req: with(func(e *cloudv1.ExistingCluster, _ *cloudv1.ClusterSpec) { e.Policy = 9 })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := protovalidate.Validate(tt.req); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
