package rollout

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

func networkWithCode(name string, tier cloud.ClusterTier, phase, target string) cloud.Network {
	n := cloud.Network{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-acme", Generation: 1, Labels: map[string]string{cloudstore.LabelID: name}}, Spec: cloud.NetworkSpec{Tier: tier}, Status: cloud.NetworkStatus{Phase: cloud.NetworkPhaseReady, ObservedGeneration: 1, CodeChange: &cloud.CodeChange{Phase: phase}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: metav1.NewTime(now.Add(-48 * time.Hour))}}}}
	setNetworkTarget(&n, target)
	return n
}

func setNetworkTarget(n *cloud.Network, target string) {
	raw, _ := json.Marshal(map[string]any{"codeChange": map[string]any{"target": target}})
	n.Status.ContractStatus = &runtime.RawExtension{Raw: raw}
}

func TestNetworkCodeColdBuildCannotSkipUnobservedCanary(t *testing.T) {
	dev := networkWithCode("dev", cloud.ClusterTier2, convert.CodeChangeDone, "old/dev")
	prod := networkWithCode("prod", cloud.ClusterTier0, convert.CodeChangePending, "new/prod")
	in := input(cluster("unchanged-dev", cloud.ClusterTier2, "1.36"), cluster("unchanged-prod", cloud.ClusterTier0, "1.36"))
	in.CodeEpoch = "new"
	in.Networks = []cloud.Network{dev, prod}
	in.Channel.Spec.Policy.MaxInProgress = 1
	in.Channel.Spec.Policy.Soak = metav1.Duration{Duration: time.Hour}
	in.Channel.Status.CodeChanges = &cloud.ChannelReleaseMoves{Done: true, Release: "old"}
	plan := ComputeCode(in)
	if plan.Done || len(plan.Admitted) != 0 || !strings.Contains(plan.Waiting, "every participating network") {
		t.Fatalf("Tier0 admitted before Tier2 observed current render: %+v", plan)
	}
	// Tier2 now publishes its changed target. Tier0 still cannot consume a slot.
	in.Networks[0].Status.CodeChange.Phase = convert.CodeChangePending
	setNetworkTarget(&in.Networks[0], "new/dev")
	plan = ComputeCode(in)
	if !slices.Equal(plan.Admitted, []string{NetworkCodeKey(&in.Networks[0])}) {
		t.Fatalf("canary was not first: %+v", plan)
	}
	in.Networks[0].Status.CodeChange.Phase = convert.CodeChangeApplying
	if got := ComputeCode(in); len(got.Admitted) != 0 || got.Done {
		t.Fatalf("passed in-flight network: %+v", got)
	}
	in.Networks[0].Status.CodeChange.Phase = convert.CodeChangeDone
	in.Networks[0].Status.CodeChange.FinishTime = &metav1.Time{Time: now}
	if got := ComputeCode(in); len(got.Admitted) != 0 || !strings.Contains(got.Waiting, "soak") {
		t.Fatalf("network success did not start soak: %+v", got)
	}
	in.Now = now.Add(time.Hour + time.Second)
	if got := ComputeCode(in); !slices.Equal(got.Admitted, []string{NetworkCodeKey(&in.Networks[1])}) {
		t.Fatalf("Tier0 not admitted after network canary soak: %+v", got)
	}
}

func TestNetworkCodeDependencyDoesNotConsumeParentSlot(t *testing.T) {
	n := networkWithCode("main", cloud.ClusterTier0, convert.CodeChangePending, "new/main")
	c := withCode(cluster("child", cloud.ClusterTier0, "1.36"), convert.CodeChangePending)
	c.Spec.Network.NetworkRef.Name = n.Name
	in := input(c)
	in.CodeEpoch = "new"
	in.Networks = []cloud.Network{n}
	in.Channel.Spec.Policy.MaxInProgress = 1
	plan := ComputeCode(in)
	if !slices.Equal(plan.Admitted, []string{NetworkCodeKey(&n)}) {
		t.Fatalf("dependent cluster consumed the only network slot: %+v", plan)
	}
	in.Networks[0].Status.CodeChange.Phase = convert.CodeChangeDone
	in.Networks[0].Status.CodeChange.FinishTime = &metav1.Time{Time: now}
	if got := ComputeCode(in); !slices.Equal(got.Admitted, []string{"tenant-acme/child"}) {
		t.Fatalf("cluster did not follow completed parent: %+v", got)
	}
}

func TestNetworkCodePausedChannelAndSharedAdmission(t *testing.T) {
	n := networkWithCode("shared", cloud.ClusterTier1, convert.CodeChangePending, "new/shared")
	a, b := cluster("a", cloud.ClusterTier1, "1.36"), cluster("b", cloud.ClusterTier1, "1.36")
	a.Spec.ReleaseChannel = cloud.ReleaseChannelAlpha
	key := NetworkCodeKey(&n)
	channel := func(name string) *cloud.ReleaseChannel {
		return &cloud.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: cloud.ReleaseChannelStatus{CodeChanges: &cloud.ChannelReleaseMoves{Release: "new", Tier: cloud.ClusterTier1, Admitted: []string{key}}}}
	}
	channels := map[string]*cloud.ReleaseChannel{"alpha": channel("alpha"), "stable": channel("stable")}
	if ok, why := NetworkCodeAdmitted(&n, []cloud.Cluster{a, b}, channels, nil, now); !ok {
		t.Fatal(why)
	}
	channels["stable"].Spec.Paused = true
	if ok, why := NetworkCodeAdmitted(&n, []cloud.Cluster{a, b}, channels, nil, now); ok || !strings.Contains(why, "paused") {
		t.Fatalf("paused attached channel admitted shared network: %t %s", ok, why)
	}
	channels["stable"].Spec.Paused = false
	channels["stable"].Status.CodeChanges.Admitted = nil
	if ok, _ := NetworkCodeAdmitted(&n, []cloud.Cluster{a, b}, channels, nil, now); ok {
		t.Fatal("one channel's admission bypassed another")
	}
	channels["stable"].Status.CodeChanges.Admitted = []string{key}
	setNetworkTarget(&n, "new/different-target")
	if ok, _ := NetworkCodeAdmitted(&n, []cloud.Cluster{a, b}, channels, nil, now); ok {
		t.Fatal("stale target admission reused")
	}
	// A paused channel's freshly computed plan must also contain no admissions.
	in := input()
	in.CodeEpoch = "new"
	in.Networks = []cloud.Network{n}
	in.Channel.Spec.Paused = true
	if got := ComputeCode(in); len(got.Admitted) != 0 {
		t.Fatal("paused plan starts network code")
	}
}

func TestNetworkDiscoveryExcludesHeldAndNonProvisionableMembers(t *testing.T) {
	for _, kind := range []string{"frozen", "paused", "deleting", "imported", "provisioning", "failed"} {
		t.Run(kind, func(t *testing.T) {
			dev := networkWithCode("dev", cloud.ClusterTier2, convert.CodeChangeDone, "old/dev")
			prod := networkWithCode("prod", cloud.ClusterTier0, convert.CodeChangePending, "new/prod")
			in := input()
			in.CodeEpoch = "new"
			in.Channel.Spec.Policy.Soak = metav1.Duration{}
			switch kind {
			case "frozen":
				in.Holds = []cloud.ReleaseHold{{Spec: cloud.ReleaseHoldSpec{Platform: true, Scoped: true, Rules: []cloud.FreezeRule{{NetworkID: "dev", Frozen: true}}}}}
			case "paused":
				dev.Annotations = map[string]string{"crossplane.io/paused": "true"}
			case "deleting":
				dev.DeletionTimestamp = &metav1.Time{Time: now}
			case "imported":
				dev.Status.Phase = cloud.NetworkPhaseImported
			case "provisioning":
				dev.Status.Phase = cloud.NetworkPhaseProvisioning
			case "failed":
				dev.Status.Phase = cloud.NetworkPhaseFailed
			}
			in.Networks = []cloud.Network{dev, prod}
			if got := ComputeCode(in); !slices.Equal(got.Admitted, []string{NetworkCodeKey(&prod)}) {
				t.Fatalf("excluded member blocks allowed network: %+v", got)
			}
		})
	}
}

func TestNetworkCanaryRejectsOldReadyObservations(t *testing.T) {
	n := networkWithCode("dev", cloud.ClusterTier2, convert.CodeChangeDone, "new/dev")
	n.Generation = 2
	if networkCodeReady(&n) {
		t.Fatal("old native observed generation credited")
	}
	n.Status.ObservedGeneration = 2
	if networkCodeReady(&n) {
		t.Fatal("old Ready condition generation credited")
	}
	n.Status.Conditions[0].ObservedGeneration = 2
	n.Status.ContractStatus = &runtime.RawExtension{Raw: []byte(`{"codeChange":{"target":"new/dev"},"managedResources":[{"generation":"2","observedGeneration":"1","state":"MANAGED_RESOURCE_STATE_READY"}]}`)}
	if networkCodeReady(&n) {
		t.Fatal("old provider Ready credited after changed managed resource generation")
	}
	n.Status.ContractStatus = &runtime.RawExtension{Raw: []byte(`{"codeChange":{"target":"new/dev"},"managedResources":[{"generation":"2","observedGeneration":"2","state":"MANAGED_RESOURCE_STATE_READY"}]}`)}
	if !networkCodeReady(&n) {
		t.Fatal("current-generation provider convergence not accepted")
	}
	// A provider that publishes no observedGeneration (provider-aws v2.9.0):
	// the network's reconciler proved the observation came after its apply
	// before it marked the change Done, so READY is the provider's word.
	n.Status.ContractStatus = &runtime.RawExtension{Raw: []byte(`{"codeChange":{"target":"new/dev"},"managedResources":[{"generation":"3","state":"MANAGED_RESOURCE_STATE_READY"}]}`)}
	if !networkCodeReady(&n) {
		t.Fatal("a Ready resource of a provider without observedGeneration held the network back")
	}
	n.Status.ContractStatus = &runtime.RawExtension{Raw: []byte(`{"codeChange":{"target":"new/dev"},"managedResources":[{"generation":"3","state":"MANAGED_RESOURCE_STATE_CREATING"}]}`)}
	if networkCodeReady(&n) {
		t.Fatal("a resource that is not Ready credited without observedGeneration")
	}
}

func TestNetworkFailuresConsumeChannelFailureBudget(t *testing.T) {
	first := networkWithCode("canary-a", cloud.ClusterTier2, convert.CodeChangeHalted, "new/a")
	second := networkWithCode("canary-b", cloud.ClusterTier2, convert.CodeChangeHalted, "new/b")
	prod := networkWithCode("prod", cloud.ClusterTier0, convert.CodeChangePending, "new/prod")
	in := input()
	in.CodeEpoch = "new"
	in.Channel.Spec.Policy.MaxFailed = 1
	in.Networks = []cloud.Network{first, second, prod}
	if got := ComputeCode(in); !got.Halted || len(got.Admitted) != 0 || got.Done {
		t.Fatalf("failed network canaries did not halt production admission: %+v", got)
	}
}
