package aws_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

// The Helm gate governs Helm Release objects only: whatever it answers, a
// component's IAM objects (role, policy, attachments, pod identity) carry
// no gate, so an unsettled Release of the cluster never holds them back.
func TestTheReleaseGateIsOnlyOnHelmReleases(t *testing.T) {
	plan := platformPlan()
	plan.ReleaseGate = func(context.Context, *unstructured.Unstructured) sequence.Decision {
		return sequence.Decision{Hold: "Helm release other to settle"}
	}

	set := crossplaneaws.PlatformSet(plan)

	releases, others := 0, 0
	for _, item := range set {
		isRelease := item.Object.GetObjectKind().GroupVersionKind().Kind == crossplaneaws.HelmReleaseGVK.Kind
		if isRelease {
			releases++
			if item.Gate == nil {
				t.Errorf("Release %s has no gate", item.Object.GetName())
			}
			continue
		}
		others++
		if item.Gate != nil {
			t.Errorf("%T %s is gated by the Helm gate", item.Object, item.Object.GetName())
		}
	}
	if releases == 0 || others == 0 {
		t.Fatalf("the plan has %d Releases and %d other objects", releases, others)
	}
}
