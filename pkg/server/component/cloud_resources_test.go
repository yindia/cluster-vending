package component

import (
	"testing"

	"google.golang.org/protobuf/proto"

	core "github.com/yindia/cluster-vending/pkg/component"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The catalog shows the IAM role the platform creates for a component.
func TestDefinitionMessageShowsTheIAMRole(t *testing.T) {
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`
	e := entry{Definition: core.Definition{Name: "loki", CloudResources: core.CloudResources{IAMRole: &core.IAMRole{
		Access: core.PodIdentity, Policy: policy, ManagedPolicyARNs: []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"},
		ServiceAccounts: []core.ServiceAccount{{Namespace: "logging", Name: "loki"}},
	}}}}
	got, err := definitionMessage(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &cloudv1.ComponentCloudResources{IamRole: &cloudv1.ComponentIamRole{
		Access: cloudv1.ComponentIamAccess_COMPONENT_IAM_ACCESS_POD_IDENTITY, Policy: policy,
		ManagedPolicyArns: []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"},
		ServiceAccounts:   []*cloudv1.ComponentServiceAccount{{Namespace: "logging", Name: "loki"}},
	}}
	if !proto.Equal(got.GetCloudResources(), want) {
		t.Fatalf("cloud_resources = %v, want %v", got.GetCloudResources(), want)
	}
}

func TestCloudResourcesMessage(t *testing.T) {
	if got := cloudResourcesMessage(core.CloudResources{}); got != nil {
		t.Fatalf("no role = %v, want nil", got)
	}
	irsa := cloudResourcesMessage(core.CloudResources{IAMRole: &core.IAMRole{Access: core.IRSA}})
	if irsa.GetIamRole().GetAccess() != cloudv1.ComponentIamAccess_COMPONENT_IAM_ACCESS_IRSA {
		t.Fatalf("access = %v, want IRSA", irsa.GetIamRole().GetAccess())
	}
}
