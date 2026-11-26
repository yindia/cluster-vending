package cluster

import (
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/component"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestPlatformStatusReportsExactComponentIAMFailures(t *testing.T) {
	iam := crossplaneaws.ComponentIAMPlan{
		Namespace: "tenant-acme", ClusterPrefix: "inference-dev-eece54a0", Component: "karpenter",
		Access: crossplaneaws.ComponentAccessPodIdentity, Policy: `{"Version":"2012-10-17","Statement":[]}`,
	}
	otherIAM := iam
	otherIAM.Component = "other"
	plan := crossplaneaws.PlatformPlan{ClusterPrefix: iam.ClusterPrefix, Components: []crossplaneaws.PlatformComponentPlan{
		{Name: "karpenter", IAM: &iam, Main: crossplaneaws.HelmReleasePlan{Name: "karpenter"}},
		{Name: "other", IAM: &otherIAM, Main: crossplaneaws.HelmReleasePlan{Name: "other"}},
	}}
	in := component.PlatformInput{Entries: []component.PlatformEntry{
		{Definition: component.Definition{Name: "karpenter"}},
		{Definition: component.Definition{Name: "other"}},
	}}
	release := plan.ReleaseObjectNames(plan.Components[0])[0]
	res := sequence.Result{Waiting: []sequence.Waiting{{
		Ref: managed.Ref{Kind: crossplaneaws.HelmReleaseGVK.Kind, Name: release},
		For: []string{"RolePolicyAttachment missing"},
	}}}
	const providerError = "create failed: LimitExceeded: policy exceeds 6144 characters"
	for _, tt := range []struct {
		name, kind, object string
		state              cloudv1.ManagedResourceState
		failed             bool
	}{
		{"failed policy", "Policy", crossplaneaws.ComponentPolicyName(iam), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, true},
		{"failed role", "Role", crossplaneaws.ComponentRoleName(iam), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, true},
		{"kind omitted", "", crossplaneaws.ComponentPolicyName(iam), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, true},
		{"missing policy", "Policy", crossplaneaws.ComponentPolicyName(iam), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING, false},
		{"pending policy", "Policy", crossplaneaws.ComponentPolicyName(iam), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING, false},
		{"waiting policy", "Policy", crossplaneaws.ComponentPolicyName(iam), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING, false},
		{"other component policy", "Policy", crossplaneaws.ComponentPolicyName(otherIAM), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, false},
		{"unrelated policy", "Policy", "unrelated-policy", cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, false},
		{"same name wrong kind", "Release", crossplaneaws.ComponentPolicyName(iam), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			views := []*cloudv1.ManagedResource{{Kind: tt.kind, Name: tt.object, State: tt.state, Message: providerError}}
			components, objects := platformStatus(in, plan, nil, res, views, nil)
			got := components[0]
			if tt.failed {
				kind := tt.kind
				if kind == "" {
					kind = "Policy"
				}
				want := kind + " " + tt.object + ": " + providerError
				if got.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED || got.GetMessage() != want {
					t.Fatalf("component = %v, want FAILED: %s", got, want)
				}
			} else if got.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING || !strings.Contains(got.GetMessage(), "RolePolicyAttachment missing") {
				t.Fatalf("ordinary attachment wait replaced by unrelated failure: %v", got)
			}
			if objects["karpenter"] != "" || got.GetChart() != "" {
				t.Fatalf("IAM status must not select a Helm detail object: %v, %v", objects, got)
			}
			if tt.name == "other component policy" && components[1].GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED {
				t.Fatalf("owning component did not report its policy failure: %v", components[1])
			}
		})
	}
}
