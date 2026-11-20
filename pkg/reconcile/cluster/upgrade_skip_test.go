package cluster

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A Kubernetes upgrade whose platform components keep their version and
// values: the step is reported skipped, once the pass that applies it has
// found that no component changed. The gate (what the step waits for) is
// not touched.
func TestUpgradeReportsSkippedComponentSteps(t *testing.T) {
	const ready, yes = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
	asked := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	started := timestamppb.New(asked.Add(-time.Hour))
	row := &model.Cluster{Resource: model.Resource{Base: model.Base{ID: "c-1", UpdatedAt: asked}, State: StateRunning, Generation: 3, AppliedGeneration: 3}}
	views := []*cloudv1.ManagedResource{upgradeView("Cluster", "prod", ready, yes, ""), upgradeView(KindNodeGroup, "prod-default", ready, yes, ""),
		upgradeView("Addon", "prod-addon-coredns", ready, yes, "")}
	onTarget := upgrade{target: "1.35", controlPlane: "1.35", nodes: []string{"1.35"},
		nodeGroups: map[string]string{"prod-default": "1.35"}, nodeSpecs: map[string]string{"prod-default": "1.35"}}
	at := func(step cloudv1.ClusterUpgradePhase, applied bool, skipped ...cloudv1.ClusterUpgradePhase) *cloudv1.ClusterUpgrade {
		return &cloudv1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Phase: step, StartTime: started, Applied: applied, SkippedSteps: skipped}
	}
	unchanged := componentsState{platformReady: true, tenantReady: true, platformUnchanged: true, tenantUnchanged: true}
	changed := componentsState{platformReady: true, tenantReady: true}

	for _, tt := range []struct {
		name    string
		stored  *cloudv1.ClusterUpgrade
		comps   componentsState
		phase   cloudv1.ClusterUpgradePhase
		skipped []cloudv1.ClusterUpgradePhase
		message string
	}{
		{name: "entering the platform step: nothing known yet", stored: at(stepAddonsPhase, true), comps: unchanged, phase: stepPlatformComps},
		{name: "the pass that applies the platform step: nothing changed", stored: at(stepPlatformComps, false), comps: unchanged,
			phase: stepPlatformComps, skipped: []cloudv1.ClusterUpgradePhase{stepPlatformComps}, message: "component versions unchanged"},
		{name: "the pass that applies it: a component changed", stored: at(stepPlatformComps, false), comps: changed, phase: stepPlatformComps},
		{name: "later passes keep what was found", stored: at(stepPlatformComps, true, stepPlatformComps), comps: changed,
			phase: stepTenantComps, skipped: []cloudv1.ClusterUpgradePhase{stepPlatformComps}},
		{name: "a step that changed things is not skipped later because the Releases now match", stored: at(stepPlatformComps, true), comps: unchanged,
			phase: stepTenantComps},
		{name: "the tenant step: nothing changed", stored: at(stepTenantComps, false, stepPlatformComps), comps: unchanged,
			phase: stepTenantComps, skipped: []cloudv1.ClusterUpgradePhase{stepPlatformComps, stepTenantComps}, message: "component versions unchanged"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := onTarget.status(row, views, tt.stored, tt.comps)

			if got == nil || got.GetPhase() != tt.phase || !slices.Equal(got.GetSkippedSteps(), tt.skipped) || !strings.Contains(got.GetMessage(), tt.message) {
				t.Fatalf("%v, want %s skipping %v with %q", got, tt.phase, tt.skipped, tt.message)
			}
		})
	}
}
