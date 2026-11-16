package controller

import (
	"strings"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

func TestClusterControllerReportsUpgradeBlockedByFreeze(t *testing.T) {
	w := newWorld(t)
	w.readyNetwork(t)
	w.must(t, w.k.Create(t.Context(), handMadeCluster(t, "prod", "net")))
	recorder := events.NewFakeRecorder(100)
	w.deps.Events = NewEvents(recorder, w.k)
	r := NewClusterReconciler(w.deps)
	for range 2 {
		_, err := r.Reconcile(t.Context(), request("prod"))
		w.must(t, err)
	}
	obj := w.cluster(t, "prod")
	id := obj.Labels[cloudstore.LabelID]
	providers := &awseks.ClusterList{}
	w.must(t, w.k.List(t.Context(), providers, client.MatchingLabels{crossplane.LabelCluster: id}))
	if len(providers.Items) != 1 {
		t.Fatal("cluster provider missing")
	}
	eks := &providers.Items[0]
	eks.Status.AtProvider.Version = ptr.To("1.33")
	w.must(t, w.k.Status().Update(t.Context(), eks))
	obj.Spec.ControlPlane.KubernetesVersion = "1.34"
	obj.Generation++
	w.must(t, w.k.Update(t.Context(), obj))
	hold := &cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: platform.FreezeName}, Spec: cloud.ReleaseHoldSpec{Platform: true, Reason: "provider maintenance", By: "operator"}}
	w.must(t, w.k.Create(t.Context(), hold))
	for range 3 {
		_, err := r.Reconcile(t.Context(), request("prod"))
		w.must(t, err)
	}
	obj = w.cluster(t, "prod")
	condition := meta.FindStatusCondition(obj.Status.Conditions, reconcilecluster.ConditionUpgradePaused)
	if condition == nil || condition.Status != metav1.ConditionTrue || obj.Status.Upgrade == nil || !strings.Contains(obj.Status.Message, "upgrade from 1.33 to 1.34 paused") {
		t.Fatalf("blocked upgrade not persisted on CR: %+v", obj.Status)
	}
	count := 0
	for len(recorder.Events) > 0 {
		message := <-recorder.Events
		if strings.Contains(message, "Normal UpgradePaused ") {
			count++
			if !strings.Contains(message, "provider maintenance") {
				t.Fatal(message)
			}
		}
	}
	if count != 1 {
		t.Fatalf("Kubernetes UpgradePaused event count %d", count)
	}
	w.must(t, w.k.Delete(t.Context(), hold))
	_, err := r.Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	obj = w.cluster(t, "prod")
	if meta.FindStatusCondition(obj.Status.Conditions, reconcilecluster.ConditionUpgradePaused) != nil || strings.Contains(obj.Status.Message, "paused:") {
		t.Fatal("resume retained paused upgrade status")
	}
}
