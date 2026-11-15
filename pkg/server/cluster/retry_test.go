package cluster

import (
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const (
	phaseFailed = cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED
	phaseReady  = cloudv1.ComponentPhase_COMPONENT_PHASE_READY
)

func failingCluster() ([]retryComponent, *cloudv1.ClusterRollout) {
	comps := []retryComponent{
		{Name: "kyverno", Kind: cloudv1.RetryItemKind_RETRY_ITEM_KIND_PLATFORM_COMPONENT, Phase: phaseFailed,
			Chart: "kyverno", Message: "chart kyverno: create failed: connection reset by peer"},
		{Name: "karpenter", Kind: cloudv1.RetryItemKind_RETRY_ITEM_KIND_PLATFORM_COMPONENT, Phase: phaseFailed,
			Message: "component karpenter uses IRSA, which cluster demo has off: set spec.aws.irsa to true on the cluster first"},
		{Name: "istiod", Kind: cloudv1.RetryItemKind_RETRY_ITEM_KIND_PLATFORM_COMPONENT, Phase: phaseReady},
		{Name: "gpu-operator", Kind: cloudv1.RetryItemKind_RETRY_ITEM_KIND_TENANT_COMPONENT, Phase: phaseFailed,
			Chart: "gpu-operator", Message: "chart gpu-operator: timed out"},
	}
	rollout := &cloudv1.ClusterRollout{Channel: "alpha", ToVersion: "1.37", Result: cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_TIMED_OUT}
	return comps, rollout
}

func byName(items []*cloudv1.RetryItem) map[string]*cloudv1.RetryItem {
	out := map[string]*cloudv1.RetryItem{}
	for _, it := range items {
		out[it.GetName()] = it
	}
	return out
}

// With nothing named, everything that failed: a chart that failed is
// tried again, a precondition says what to fix, the failed upgrade is
// tried again, and what is fine is left out.
func TestRetryPlanEverythingFailed(t *testing.T) {
	comps, rollout := failingCluster()

	items, retry, err := retryPlan(comps, rollout, nil, false)

	if err != nil {
		t.Fatal(err)
	}
	got := byName(items)
	if len(got) != 4 || got["istiod"] != nil {
		t.Fatalf("items = %v", items)
	}
	if got["kyverno"].GetAction() != cloudv1.RetryAction_RETRY_ACTION_RETRY || got["gpu-operator"].GetAction() != cloudv1.RetryAction_RETRY_ACTION_RETRY {
		t.Errorf("failed charts not retried: %v", items)
	}
	if fix := got["karpenter"]; fix.GetAction() != cloudv1.RetryAction_RETRY_ACTION_FIX || !strings.Contains(fix.GetMessage(), "spec.aws.irsa") {
		t.Errorf("precondition = %v, want FIX saying what to change", fix)
	}
	if up := got[upgradeItem]; up.GetAction() != cloudv1.RetryAction_RETRY_ACTION_RETRY || up.GetKind() != cloudv1.RetryItemKind_RETRY_ITEM_KIND_UPGRADE {
		t.Errorf("upgrade = %v", up)
	}
	if !retry.Upgrade || len(retry.Components) != 2 || !retry.Has("kyverno") || !retry.Has("gpu-operator") || retry.Has("karpenter") {
		t.Errorf("request = %+v: only what is retried", retry)
	}
}

// Named ones only; a named one that has not failed says so; an unknown
// name is an error; the upgrade only with upgrade: true.
func TestRetryPlanNamed(t *testing.T) {
	comps, rollout := failingCluster()

	items, retry, err := retryPlan(comps, rollout, []string{"istiod", "kyverno"}, false)

	if err != nil {
		t.Fatal(err)
	}
	got := byName(items)
	if len(got) != 2 || got[upgradeItem] != nil {
		t.Fatalf("items = %v", items)
	}
	if ok := got["istiod"]; ok.GetAction() != cloudv1.RetryAction_RETRY_ACTION_NOTHING || !strings.Contains(ok.GetMessage(), "istiod is Ready") {
		t.Errorf("istiod = %v", ok)
	}
	if retry.Upgrade || len(retry.Components) != 1 || !retry.Has("kyverno") {
		t.Errorf("request = %+v", retry)
	}
	if _, _, err := retryPlan(comps, rollout, []string{"nope"}, false); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("unknown component: %v", err)
	}
	items, retry, _ = retryPlan(comps, nil, nil, true)
	if up := byName(items)[upgradeItem]; up.GetAction() != cloudv1.RetryAction_RETRY_ACTION_NOTHING || retry.Upgrade {
		t.Errorf("no failed upgrade: %v, %+v", up, retry)
	}
}

// A cluster where nothing failed: no items, nothing to record.
func TestRetryPlanNothingFailed(t *testing.T) {
	comps := []retryComponent{{Name: "istiod", Kind: cloudv1.RetryItemKind_RETRY_ITEM_KIND_PLATFORM_COMPONENT, Phase: phaseReady}}

	items, retry, err := retryPlan(comps, nil, nil, false)

	if err != nil || len(items) != 0 || retry.Upgrade || len(retry.Components) != 0 {
		t.Fatalf("items = %v, request = %+v, err = %v", items, retry, err)
	}
}
