package reconcile

import (
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// A missing object may be created again when it cannot become a second
// cloud resource: its kind is named by the renderer (Recreatable), or its
// cloud identity is recorded, so the new object is created with it and
// takes the existing resource over. One missing object with neither keeps
// everything as it is.
func TestRepairableByKindOrByRecordedIdentity(t *testing.T) {
	const (
		missing = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING
		ready   = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
	)
	role := model.ManagedResource{APIVersion: "iam.aws.m.upbound.io/v1beta1", Kind: "Role", Name: "node-role"}
	vpc := model.ManagedResource{APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "VPC", Name: "vpc"}
	named := vpc
	named.ExternalName = "vpc-0abc"
	tests := []struct {
		name    string
		records []model.ManagedResource
		views   []*cloudv1.ManagedResource
		want    int
	}{
		{"a kind the renderer names", []model.ManagedResource{role}, []*cloudv1.ManagedResource{{State: missing}}, 1},
		{"an ID AWS assigns, recorded", []model.ManagedResource{named}, []*cloudv1.ManagedResource{{State: missing}}, 1},
		{"an ID AWS assigns, never recorded", []model.ManagedResource{vpc}, []*cloudv1.ManagedResource{{State: missing}}, 0},
		{"one of two cannot be created again", []model.ManagedResource{role, vpc}, []*cloudv1.ManagedResource{{State: missing}, {State: missing}}, 0},
		{"both can", []model.ManagedResource{role, named}, []*cloudv1.ManagedResource{{State: missing}, {State: missing}}, 2},
		{"a recorded one while another is paused", []model.ManagedResource{named, role},
			[]*cloudv1.ManagedResource{{State: missing}, {State: ready, Paused: true}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repairable(tt.records, tt.views)

			if len(got) != tt.want {
				t.Fatalf("repairable = %v, want %d objects", got, tt.want)
			}
		})
	}
}

// The set that repairs carries, on each item that stands for a missing
// object, the cloud identity recorded for it; nothing else is touched.
func TestNamedGivesMissingObjectsTheirRecordedIdentity(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	set := sequence.Of(
		&awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "vpc"}},
		&awsec2.Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "subnet-a"}},
		&awsiam.Role{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "vpc"}}, // same name, another kind
	)
	missing := []model.ManagedResource{
		{APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "VPC", Namespace: "tenant-acme", Name: "vpc", ExternalName: "vpc-0abc"},
		{APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "Subnet", Namespace: "tenant-acme", Name: "gone", ExternalName: "subnet-0"},
		{APIVersion: "iam.aws.m.upbound.io/v1beta1", Kind: "Role", Namespace: "tenant-acme", Name: "vpc"}, // no identity recorded
	}

	got, err := Named(set, missing, scheme)

	if err != nil {
		t.Fatal(err)
	}
	want := []string{"vpc-0abc", "", ""}
	for i, item := range got {
		if item.ExternalName != want[i] {
			t.Errorf("item %d (%T %s): external name %q, want %q", i, item.Object, item.Object.GetName(), item.ExternalName, want[i])
		}
	}
	if set[0].ExternalName != "" {
		t.Error("Named changed the set it was given")
	}
}
