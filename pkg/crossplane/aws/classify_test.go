package aws_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// Every field the cluster renderer outputs under spec.forProvider is
// classified: a new field needs a decision before it ships.
func TestEveryRenderedFieldIsClassified(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	plan := clusterPlan() // version, taints, labels, admin principal
	plan.ServiceCIDR = "172.20.0.0/16"
	plan.LogTypes = []string{"audit"}
	plan.SupportType = "STANDARD"
	plan.Tags = map[string]string{"team": "platform"}
	plan.NodePool.DiskSizeGB = 50
	allOptions(&plan)
	plan.Karpenter, plan.AccountID = true, "111122223333" // the node role, access entry, interruption queue and rules
	plan.ResultsBucket = true                             // the results bucket and its settings
	plan.Addons = []crossplaneaws.AddonPlan{
		{Name: "coredns", Version: "v1.12.0-eksbuild.1", ConfigurationValues: `{"replicaCount":3}`},
		{Name: crossplaneaws.AddonEBSCSI}, {Name: crossplaneaws.AddonEFSCSI}, {Name: crossplaneaws.AddonFSxCSI},
		{Name: crossplaneaws.AddonMountpointS3, S3BucketNames: []string{"data"}},
	}
	seen := 0
	for _, obj := range crossplaneaws.ClusterResources(plan) {
		u, err := kube.ApplyObject(obj, scheme)
		if err != nil {
			t.Fatal(err)
		}
		if u.GetKind() == "Usage" {
			continue
		}
		forProvider, _, _ := unstructured.NestedMap(u.Object, "spec", "forProvider")
		for path := range crossplaneaws.FlattenFields(forProvider) {
			seen++
			if _, ok := crossplaneaws.ClassifyField(u.GetKind(), path); !ok {
				t.Errorf("%s forProvider.%s is rendered but not classified (pkg/crossplane/aws/classify.go)", u.GetKind(), path)
			}
		}
	}
	if seen < 60 {
		t.Fatalf("only %d fields checked: the render is not the maximal one", seen)
	}
}

func TestClassifyField(t *testing.T) {
	for _, tt := range []struct {
		kind, path string
		want       crossplaneaws.ChangeClass
		ok         bool
	}{
		{"Cluster", "version", crossplaneaws.ChangeDisruptive, true},
		{"Cluster", "tags.team", crossplaneaws.ChangeInPlace, true},
		{"NodeGroup", "instanceTypes", crossplaneaws.ChangeImmutable, true},
		{"NodeGroup", "scalingConfig.maxSize", crossplaneaws.ChangeInPlace, true},
		{"Addon", "addonVersion", crossplaneaws.ChangeDisruptive, true},
		{"NodeGroup", "launchTemplate.id", crossplaneaws.ChangeImmutable, true},
		{"NodeGroup", "launchTemplate.version", crossplaneaws.ChangeDisruptive, true},
		{"Cluster", "encryptionConfig.provider.keyArn", crossplaneaws.ChangeAddOnly, true},
		{"Cluster", "vpcConfig.securityGroupIds", crossplaneaws.ChangeImmutable, true},
		{"AccessPolicyAssociation", "accessScope.namespaces", crossplaneaws.ChangeImmutable, true},
		{"NodeGroup", "remoteAccess.ec2SshKey", 0, false},
		{"Rule", "eventBusName", crossplaneaws.ChangeAddOnly, true},
		{"Target", "eventBusName", crossplaneaws.ChangeAddOnly, true},
		{"Unknown", "x", 0, false},
	} {
		got, ok := crossplaneaws.ClassifyField(tt.kind, tt.path)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s %s = %v %v, want %v %v", tt.kind, tt.path, got, ok, tt.want, tt.ok)
		}
	}
}
