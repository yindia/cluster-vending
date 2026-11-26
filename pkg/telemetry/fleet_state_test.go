package telemetry

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/helmrel"
)

func helmRelease(name string, annotations map[string]string, state string) *unstructured.Unstructured {
	r := &unstructured.Unstructured{}
	r.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	r.SetName(name)
	r.SetNamespace("tenant-a")
	r.SetAnnotations(annotations)
	if state != "" {
		_ = unstructured.SetNestedField(r.Object, state, "status", "atProvider", "state")
	}
	return r
}

func fleetScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := cloud.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gv := crossplaneaws.HelmReleaseGVK.GroupVersion()
	scheme.AddKnownTypeWithName(crossplaneaws.HelmReleaseGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gv.WithKind(crossplaneaws.HelmReleaseGVK.Kind+"List"), &unstructured.UnstructuredList{})
	return scheme
}

func TestFleetStateSeriesAreDerivedFromStatusAndBounded(t *testing.T) {
	now := time.Unix(1800000000, 0)
	at := func(d time.Duration) *metav1.Time { v := metav1.NewTime(now.Add(-d)); return &v }
	contract := `{"managedResources":[{"kind":"Cluster","state":"MANAGED_RESOURCE_STATE_READY"},` +
		`{"kind":"NodeGroup","state":"MANAGED_RESOURCE_STATE_FAILED"},{"kind":"secret kind","state":"MANAGED_RESOURCE_STATE_CREATING"}]}`
	running := &cloud.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-running", Namespace: "tenant-a", CreationTimestamp: *at(48 * time.Hour)},
		Spec:       cloud.ClusterSpec{ReleaseChannel: "stable"},
		Status: cloud.ClusterStatus{Phase: cloud.ClusterPhaseRunning, CurrentKubernetesVersion: "1.33.4-eks",
			Kubernetes:      &cloud.ClusterKubernetesStatus{Ending: true},
			Upgrade:         &cloud.ClusterUpgrade{Phase: cloud.ClusterUpgradeAddons, Halted: true},
			ContractStatus:  &runtime.RawExtension{Raw: []byte(contract)},
			Deprecations:    &cloud.ClusterDeprecations{AttemptedAt: at(2 * time.Hour)},
			Recommendations: &cloud.ClusterRecommendations{AttemptedAt: at(time.Hour)},
			Results:         []cloud.ClusterResultRef{{Kind: "deprecations", AttemptedAt: at(10 * time.Minute)}},
			PlatformComponents: []cloud.PlatformComponentStatus{
				{Name: "karpenter", Phase: "Installing", Since: at(40 * time.Minute)},
				{Name: "attacker-component", Phase: "Ready"},
			}},
	}
	deleting := &cloud.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-deleting", Namespace: "tenant-a", Finalizers: []string{"keep"},
			DeletionTimestamp: at(90 * time.Minute), CreationTimestamp: *at(72 * time.Hour)},
		Status: cloud.ClusterStatus{Phase: cloud.ClusterPhaseDeleting, CurrentKubernetesVersion: "garbage",
			ContractStatus: &runtime.RawExtension{Raw: []byte(`{"teardown":{"startTime":"2027-01-01T00:00:00Z"}}`)}},
	}
	component := &cloud.Component{
		ObjectMeta: metav1.ObjectMeta{Name: "private-component", Namespace: "tenant-a", CreationTimestamp: *at(5 * time.Hour)},
		Spec:       cloud.ComponentSpec{Component: "karpenter"},
		Status: cloud.ComponentStatus{Phase: "Failed", Conditions: []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionFalse, LastTransitionTime: *at(45 * time.Minute)}}},
	}
	channel := &cloud.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: "stable"}, Status: cloud.ReleaseChannelStatus{
		RolloutTier: cloud.ClusterTier1,
		Rollout: &cloud.ChannelRollout{Halted: true,
			Members: []cloud.RolloutTierCount{{Tier: cloud.ClusterTier1, InProgress: 2, Failed: 3, Blocked: 1}},
			Blocked: []cloud.BlockedCluster{{Cluster: "tenant-a/secret-running", Gate: "MAINTENANCE_WINDOW"}, {Cluster: "x/y", Gate: "attacker gate"}}},
		ReleaseMoves: &cloud.ChannelReleaseMoves{Tier: cloud.ClusterTier2},
	}}
	definition := &cloud.ComponentDefinition{ObjectMeta: metav1.ObjectMeta{Name: "karpenter"}}
	objects := []client.Object{running, deleting, component, channel, definition,
		helmRelease("r1", map[string]string{helmrel.GaveUpAnnotation: "true"}, "failed"),
		helmRelease("r2", map[string]string{helmrel.BackoffAnnotation: now.Add(time.Minute).Format(time.RFC3339)}, "failed"),
		helmRelease("r3", nil, "pending-install"),
		helmRelease("r4", nil, "deployed"),
		helmRelease("r5", map[string]string{helmrel.BackoffAnnotation: now.Add(-time.Minute).Format(time.RFC3339)}, "failed"),
	}
	c := fake.NewClientBuilder().WithScheme(fleetScheme(t)).WithObjects(objects...).Build()
	f := NewFleet(c)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(f)
	if err := f.Refresh(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"fleet_clusters_by_version", map[string]string{"minor": "1.33", "channel": "stable"}, 1},
		{"fleet_clusters_by_version", map[string]string{"minor": "unknown", "channel": "none"}, 1},
		{"fleet_clusters_extended_support", map[string]string{"minor": "1.33"}, 1},
		{"fleet_upgrade_halted", map[string]string{"channel": "stable"}, 1},
		{"fleet_deletions", map[string]string{"kind": "cluster", "stage": "teardown"}, 1},
		{"fleet_deletions", map[string]string{"kind": "cluster", "stage": "resources"}, 0},
		{"fleet_oldest_deleting_seconds", map[string]string{"kind": "cluster"}, 5400},
		{"fleet_managed_resources_not_ready", map[string]string{"owner_kind": "cluster", "kind": "NodeGroup", "state": "failed"}, 1},
		{"fleet_managed_resources_not_ready", map[string]string{"owner_kind": "cluster", "kind": "other", "state": "creating"}, 1},
		{"fleet_component_phases", map[string]string{"scope": "platform", "component": "karpenter", "phase": "installing"}, 1},
		{"fleet_component_phases", map[string]string{"scope": "platform", "component": "other", "phase": "ready"}, 1},
		{"fleet_component_phases", map[string]string{"scope": "tenant", "component": "karpenter", "phase": "failed"}, 1},
		{"fleet_component_oldest_not_ready_seconds", map[string]string{"scope": "platform", "component": "karpenter"}, 2400},
		{"fleet_component_oldest_not_ready_seconds", map[string]string{"scope": "tenant", "component": "karpenter"}, 2700},
		{"fleet_helm_releases", map[string]string{"state": "gave_up"}, 1},
		{"fleet_helm_releases", map[string]string{"state": "backoff"}, 1},
		{"fleet_helm_releases", map[string]string{"state": "stuck"}, 1},
		{"fleet_helm_releases", map[string]string{"state": "deployed"}, 1},
		{"fleet_helm_releases", map[string]string{"state": "failed"}, 1},
		{"fleet_scan_oldest_attempt_seconds", map[string]string{"kind": "deprecations"}, 600},
		{"fleet_scan_oldest_attempt_seconds", map[string]string{"kind": "recommendations"}, 3600},
		{"fleet_scan_oldest_attempt_seconds", map[string]string{"kind": "policy"}, 48 * 3600},
		{"rollout_halted", map[string]string{"channel": "stable", "rollout": "kubernetes"}, 1},
		{"rollout_halted", map[string]string{"channel": "alpha", "rollout": "code"}, 0},
		{"rollout_current_tier", map[string]string{"channel": "stable", "rollout": "kubernetes", "tier": "tier1"}, 1},
		{"rollout_current_tier", map[string]string{"channel": "stable", "rollout": "release", "tier": "tier2"}, 1},
		{"rollout_clusters", map[string]string{"channel": "stable", "rollout": "kubernetes", "tier": "tier1", "state": "failed"}, 3},
		{"rollout_blocked_clusters", map[string]string{"channel": "stable", "gate": "MAINTENANCE_WINDOW"}, 1},
		{"rollout_blocked_clusters", map[string]string{"channel": "stable", "gate": "other"}, 1},
		{"fleet_conditions", map[string]string{"kind": "cluster", "condition": "reachable", "status": "unknown"}, 2},
		{"fleet_conditions", map[string]string{"kind": "cloudaccount", "condition": "accessverified", "status": "unknown"}, 0},
	}
	for _, check := range checks {
		if got := value(t, reg, check.name, check.labels); got != check.want {
			t.Errorf("%s%v = %v, want %v", check.name, check.labels, got, check.want)
		}
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, pair := range metric.Label {
				for _, forbidden := range []string{"secret", "private", "attacker", "tenant-a"} {
					if strings.Contains(pair.GetValue(), forbidden) {
						t.Fatalf("unbounded label: %s %v", family.GetName(), pair)
					}
				}
			}
		}
	}
}

func TestFleetBoundsDataDerivedLabels(t *testing.T) {
	var objects []client.Object
	for i := range maxMinors + 5 {
		objects = append(objects, &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("c%d", i), Namespace: "t"},
			Status: cloud.ClusterStatus{CurrentKubernetesVersion: fmt.Sprintf("1.%d", i)}})
	}
	c := fake.NewClientBuilder().WithScheme(fleetScheme(t)).WithObjects(objects...).Build()
	f := NewFleet(c)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(f)
	if err := f.Refresh(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := value(t, reg, "fleet_clusters_by_version", map[string]string{"minor": "other"}); got != 5 {
		t.Fatalf("minors past the bound: %v", got)
	}
	n, err := testutil.GatherAndCount(reg, "cluster_vending_fleet_clusters_by_version")
	if err != nil {
		t.Fatal(err)
	}
	if n != maxMinors+1 {
		t.Fatalf("series %d, want %d", n, maxMinors+1)
	}
}
