package controller

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/rollout"
)

func observedCodeNetwork(name string, tier cloud.ClusterTier, epoch, phase string) *cloud.Network {
	raw, _ := json.Marshal(map[string]any{"codeChange": map[string]any{"target": epoch + "/" + name}})
	return &cloud.Network{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-acme", Labels: map[string]string{cloudstore.LabelID: name}}, Spec: cloud.NetworkSpec{Tier: tier}, Status: cloud.NetworkStatus{Phase: cloud.NetworkPhaseReady, CodeChange: &cloud.CodeChange{Phase: phase}, ContractStatus: &runtime.RawExtension{Raw: raw}}}
}

func TestChannelPlansNetworkOnlyCodeAcrossColdStartAndSoak(t *testing.T) {
	dev := observedCodeNetwork("dev", cloud.ClusterTier2, "previous-controller", "Done")
	prod := observedCodeNetwork("prod", cloud.ClusterTier0, platform.CodeEpoch(), "Pending")
	policy := defaultPolicy
	policy.Soak = metav1.Duration{Duration: time.Hour}
	c := rolloutWorld(t, policy, dev, prod)
	first, _, _ := planRollout(t, c, rolloutNow)
	if first.CodeChanges.Done || len(first.CodeChanges.Admitted) != 0 || !strings.Contains(first.CodeChanges.Waiting, "every participating network") {
		t.Fatalf("cold channel admitted prod before dev discovery: %+v", first.CodeChanges)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(dev), dev); err != nil {
		t.Fatal(err)
	}
	dev.Status = observedCodeNetwork("dev", cloud.ClusterTier2, platform.CodeEpoch(), "Pending").Status
	if err := c.Update(t.Context(), dev); err != nil {
		t.Fatal(err)
	}
	second, _, _ := planRollout(t, c, rolloutNow)
	if !slices.Equal(second.CodeChanges.Admitted, []string{rollout.NetworkCodeKey(dev)}) {
		t.Fatalf("network-only canary was not admitted: %+v", second.CodeChanges)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(dev), dev); err != nil {
		t.Fatal(err)
	}
	dev.Status.CodeChange.Phase = "Done"
	dev.Status.CodeChange.FinishTime = &metav1.Time{Time: rolloutNow}
	if err := c.Update(t.Context(), dev); err != nil {
		t.Fatal(err)
	}
	third, _, _ := planRollout(t, c, rolloutNow.Add(30*time.Minute))
	if len(third.CodeChanges.Admitted) != 0 || !strings.Contains(third.CodeChanges.Waiting, "soak") {
		t.Fatalf("network canary did not enforce soak: %+v", third.CodeChanges)
	}
	last, _, _ := planRollout(t, c, rolloutNow.Add(time.Hour+time.Second))
	if !slices.Equal(last.CodeChanges.Admitted, []string{rollout.NetworkCodeKey(prod)}) {
		t.Fatalf("prod did not follow completed canary soak: %+v", last.CodeChanges)
	}
}

func TestNetworkCodeStatusAndManualPauseWakeChannels(t *testing.T) {
	before := observedCodeNetwork("main", cloud.ClusterTier2, "old", "Done")
	after := before.DeepCopy()
	after.Status = observedCodeNetwork("main", cloud.ClusterTier2, "new", "Done").Status
	if !networkCodeInputChanged().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("target-only observation did not wake planning")
	}
	after = before.DeepCopy()
	after.Annotations = map[string]string{"crossplane.io/paused": "true"}
	if !networkCodeInputChanged().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("manual pause did not wake planning")
	}
	if networkCodeInputChanged().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: before.DeepCopy()}) {
		t.Fatal("unchanged status retriggered planning")
	}
}
