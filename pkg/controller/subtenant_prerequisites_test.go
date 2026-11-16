package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/naas"
)

func TestSubTenantWaitsForPrerequisitesBeforeWorkloadCalls(t *testing.T) {
	for _, tt := range []struct {
		name, reason, detail string
		change               func(*cloud.Cluster)
	}{
		{"capsule installing", "CapsulePending", "webhook rollout", func(p *cloud.Cluster) {
			p.Status.PlatformComponents[0].Phase = "Installing"
			p.Status.PlatformComponents[0].Message = "webhook rollout"
		}},
		{"capsule failed", "CapsuleFailed", "chart download failed", func(p *cloud.Cluster) {
			p.Status.PlatformComponents[0].Phase = "Failed"
			p.Status.PlatformComponents[0].Message = "chart download failed"
		}},
		{"capsule name prefix insufficient", "CapsulePending", "installation", func(p *cloud.Cluster) { p.Status.PlatformComponents[0].Name = "capsule-other" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newSubTenantWorld(t)
			parent := w.parent(t)
			tt.change(parent)
			if err := w.management.Status().Update(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			r := NewSubTenantReconciler(w.deps)
			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "tenant-acme", Name: stID}})
			if err != nil || result.RequeueAfter <= 0 {
				t.Fatalf("progress must retry without an operational error: %v, %v", result, err)
			}
			status := w.sub(t).Status
			condition := meta.FindStatusCondition(status.Conditions, "PrerequisitesReady")
			if status.State != cloud.SubTenantStatePending || condition == nil || condition.Reason != tt.reason || !strings.Contains(status.Message, tt.detail) {
				t.Fatalf("unexpected pending status: %+v", status)
			}
			access := meta.FindStatusCondition(status.Conditions, "AccessReady")
			if access == nil || access.Reason != "PrerequisitesPending" {
				t.Fatalf("premature qualification diagnostic: %+v", access)
			}
			if w.clients.calls != 0 || len(w.writes) != 0 || len(w.sub(t).Finalizers) != 0 {
				t.Fatal("unready prerequisites attempted workload provisioning")
			}
			// Parent status watches/retries must resume automatically.
			parent = w.parent(t)
			parent.Status.Phase = cloud.ClusterPhaseRunning
			parent.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
			parent.Status.PlatformComponents[0] = cloud.PlatformComponentStatus{Name: "capsule", Phase: "Ready"}
			if err := w.management.Status().Update(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			w.provision(t)
			if !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "PrerequisitesReady") {
				t.Fatal("prerequisite recovery was not reflected in status")
			}
		})
	}
}

func TestSubTenantIgnoresAggregateClusterReadinessWhenCapsuleIsReady(t *testing.T) {
	for _, phase := range []cloud.ClusterPhase{cloud.ClusterPhaseUpdating, cloud.ClusterPhaseProvisioning, cloud.ClusterPhaseFailed, cloud.ClusterPhaseRunning} {
		t.Run(string(phase), func(t *testing.T) {
			w := newSubTenantWorld(t)
			parent := w.parent(t)
			parent.Generation = 7
			parent.Status.Phase = phase
			parent.Status.Message = "upgrading control plane or waiting for unrelated infrastructure"
			parent.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, ObservedGeneration: 1, Message: "node group upgrade in progress"}}
			if err := w.management.Update(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			if err := w.management.Status().Update(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			// A persisted old blocker must be replaced, not retained after recovery.
			sub := w.sub(t)
			sub.Status.State = cloud.SubTenantStatePending
			sub.Status.Message = "Waiting for cluster to become Ready"
			sub.Status.Conditions = []metav1.Condition{{Type: "PrerequisitesReady", Status: metav1.ConditionFalse, Reason: "ClusterPending", Message: sub.Status.Message}}
			if err := w.management.Status().Update(t.Context(), sub); err != nil {
				t.Fatal(err)
			}
			w.provision(t)
			status := w.sub(t).Status
			if !meta.IsStatusConditionTrue(status.Conditions, "PrerequisitesReady") || strings.Contains(status.Message, "Waiting for cluster") || status.RemoteCapsuleTenantUID == "" {
				t.Fatalf("aggregate cluster state blocked an available Capsule: %+v", status)
			}
		})
	}
}

func TestSubTenantBootstrapMessageIncludesPlatformBlocker(t *testing.T) {
	w := newSubTenantWorld(t)
	b := w.parent(t)
	b.Status.PlatformComponents[1].Message = "Policy exceeds 6144 characters"
	if err := w.management.Status().Update(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	// Remove one required bootstrap object without touching the parent readiness.
	bootstrap, err := naas.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.remote.Delete(t.Context(), bootstrap.Objects()[0]); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	status := w.sub(t).Status
	if !strings.Contains(status.Message, "Waiting for the Capsule bootstrap") || !strings.Contains(status.Message, "karpenter: Policy exceeds 6144 characters") {
		t.Fatalf("missing actionable dependency message: %s", status.Message)
	}
}
