package controller

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/naas"
)

// openAccessWorld is a world whose sub-tenant has open access on a qualified
// cluster.
func openAccessWorld(t *testing.T) *stWorld {
	t.Helper()
	w := provisionAccessWorld(t, true)
	qualifyAccessWorld(t, w)
	w.pass(t)
	materializeAccessBindings(t, w)
	w.pass(t)
	if !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") || !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "Ready") {
		t.Fatalf("world did not open access: %+v", w.sub(t).Status.Conditions)
	}
	return w
}

// The controller's access state machine (PROP-05 phase 1) on a fake cluster.
func TestSubTenantAccessStateMachine(t *testing.T) {
	day := 24 * time.Hour
	tests := []struct {
		name   string
		change func(t *testing.T, w *stWorld)
		// open: access stays open; else it closes for reason, naming text.
		open   bool
		reason string
		text   string
	}{
		{name: "a record kept for more than an hour keeps access", open: true, change: func(t *testing.T, w *stWorld) {
			writeClusterRecord(t, w, func(q *naas.ClusterQualification) {
				q.IssuedAt, q.ExpiresAt = time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour).Add(naas.MaxQualificationValidity)
			})
		}},
		{name: "a renewal in flight keeps the old record valid", open: true, change: func(t *testing.T, w *stWorld) {
			writeClusterRecord(t, w, func(q *naas.ClusterQualification) {
				q.IssuedAt = time.Now().Add(-28 * day)
				q.ExpiresAt = q.IssuedAt.Add(naas.MaxQualificationValidity)
			})
		}},
		{name: "a failed run closes access naming the check", reason: "QualificationFailed", text: naas.CheckEgressDeny, change: func(t *testing.T, w *stWorld) {
			writeClusterRecord(t, w, func(q *naas.ClusterQualification) { q.Checks[naas.CheckEgressDeny] = false })
		}},
		{name: "a group that left the list closes access", reason: "GroupNotAllowed", text: "Readers", change: func(t *testing.T, w *stWorld) {
			w.deps.NaaS = config.NaaS{AllowedGroups: []string{"Admins"}}
		}},
		{name: "a denied group closes access", reason: "GroupNotAllowed", text: "Admins", change: func(t *testing.T, w *stWorld) {
			w.deps.NaaS = config.NaaS{AllowedGroups: []string{"Admins", "Read*"}, DeniedGroups: []string{"Admins"}}
		}},
		{name: "an empty list closes access", reason: "GroupNotAllowed", text: "naas.allowedGroups", change: func(t *testing.T, w *stWorld) {
			w.deps.NaaS = config.NaaS{}
		}},
		{name: "a Capsule change ends the record", reason: "QualificationRequired", change: func(t *testing.T, w *stWorld) {
			parent := w.parent(t)
			parent.Status.PlatformComponents[0].InstalledVersion = "0.16.0"
			if err := w.management.Status().Update(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "an Istio change ends the record", reason: "QualificationRequired", change: func(t *testing.T, w *stWorld) {
			parent := w.parent(t)
			parent.Status.PlatformComponents = append(parent.Status.PlatformComponents, cloud.PlatformComponentStatus{Name: "istiod", Phase: "Ready", InstalledVersion: "1.29.0"})
			if err := w.management.Status().Update(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := openAccessWorld(t)
			tt.change(t, w)
			w.pass(t)
			sub := w.sub(t)
			access := meta.FindStatusCondition(sub.Status.Conditions, "AccessReady")
			if tt.open {
				if access.Status != "True" || sub.Status.State != cloud.SubTenantStateReady || w.tenant(t).GetLabels()[naas.LabelAccessReady] != "true" {
					t.Fatalf("access closed: %+v", access)
				}
				return
			}
			if access.Status == "True" || access.Reason != tt.reason || !strings.Contains(access.Message+sub.Status.Message, tt.text) {
				t.Fatalf("AccessReady = %+v, want reason %s naming %q", access, tt.reason, tt.text)
			}
			if w.tenant(t).GetLabels()[naas.LabelAccessReady] != "false" {
				t.Fatal("the access gate stayed open")
			}
			// A Ready sub-tenant whose access closes is Degraded (6.9), still
			// provisioned, and its Ready condition is false.
			if sub.Status.State != cloud.SubTenantStateDegraded || !meta.IsStatusConditionTrue(sub.Status.Conditions, "ProvisioningReady") || meta.IsStatusConditionTrue(sub.Status.Conditions, "Ready") {
				t.Fatalf("state = %s, conditions %+v", sub.Status.State, sub.Status.Conditions)
			}
			if tt.reason == "GroupNotAllowed" {
				if bindings, _, _ := unstructured.NestedSlice(w.tenant(t).Object, "spec", "data", "naas", "accessBindings"); len(bindings) != 0 {
					t.Fatalf("bindings still rendered for a group outside the list: %v", bindings)
				}
				if c := meta.FindStatusCondition(sub.Status.Conditions, conditionGroupsAllowed); c == nil || c.Status != "False" {
					t.Fatalf("GroupsAllowed = %+v", c)
				}
			}
			if tt.reason == "QualificationFailed" {
				if c := meta.FindStatusCondition(sub.Status.Conditions, conditionClusterQualified); c == nil || c.Reason != "QualificationFailed" {
					t.Fatalf("ClusterQualified = %+v", c)
				}
			}
			// Degraded stays Degraded while access is closed.
			w.pass(t)
			if w.sub(t).Status.State != cloud.SubTenantStateDegraded {
				t.Fatalf("Degraded flapped to %s", w.sub(t).Status.State)
			}
		})
	}
}

// The retired per-allocation record no longer opens access, and the retired
// condition is removed.
func TestSubTenantAccessRetiresTheAllocationRecord(t *testing.T) {
	w := provisionAccessWorld(t, true)
	sub := w.sub(t)
	meta.SetStatusCondition(&sub.Status.Conditions, metav1.Condition{Type: conditionRetiredQualified, Status: metav1.ConditionTrue, Reason: "QualificationVerified"})
	if err := w.management.Status().Update(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	sub = w.sub(t)
	if meta.FindStatusCondition(sub.Status.Conditions, conditionRetiredQualified) != nil {
		t.Fatal("the retired AccessQualified condition is kept")
	}
	if c := meta.FindStatusCondition(sub.Status.Conditions, conditionClusterQualified); c == nil || c.Reason != "QualificationRequired" {
		t.Fatalf("ClusterQualified = %+v", c)
	}
}
