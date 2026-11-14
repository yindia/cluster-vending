package cluster

import (
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// editObject changes the imported cluster's spec behind the API's back,
// as kubectl edit of its object does (the row is read from the object):
// another Kubernetes version and another display name. No call of the
// API can do this while the cluster is imported.
func editObject(t *testing.T, w world, id string) {
	t.Helper()
	row, err := w.store.Clusters().Get(t.Context(), worldTenant, id)
	if err != nil {
		t.Fatalf("read the cluster: %v", err)
	}
	spec, err := reconcilecluster.SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	if spec.ControlPlane == nil {
		spec.ControlPlane = &cloudv1.ControlPlaneSpec{}
	}
	spec.ControlPlane.KubernetesVersion = "1.99"
	if row.Spec, err = reconcilecluster.Encode(spec); err != nil {
		t.Fatal(err)
	}
	row.DisplayName = "edited"
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatalf("edit the cluster: %v", err)
	}
}

// An imported cluster is read-only: a spec changed on its object behind
// the API's back is not kept. The next pass puts what the import
// discovered back and says so in the status.
func TestEditOfAnImportedClusterIsIgnoredAndReported(t *testing.T) {
	w := importWorld(t, terraformCluster())
	c := importTF(t, w)
	reconcile(t, w, c.GetId())
	discovered := storedCluster(t, w, c.GetId())

	editObject(t, w, c.GetId())
	if got := storedCluster(t, w, c.GetId()).GetSpec().GetControlPlane().GetKubernetesVersion(); got != "1.99" {
		t.Fatalf("the edit did not reach the stored spec (%q): the test does not exercise the case", got)
	}
	reconcile(t, w, c.GetId())

	got := storedCluster(t, w, c.GetId())
	if v, want := got.GetSpec().GetControlPlane().GetKubernetesVersion(), discovered.GetSpec().GetControlPlane().GetKubernetesVersion(); v != want {
		t.Errorf("kubernetes version = %q, want the discovered %q", v, want)
	}
	if got.GetDisplayName() != discovered.GetDisplayName() {
		t.Errorf("display name = %q, want the discovered %q", got.GetDisplayName(), discovered.GetDisplayName())
	}
	st := got.GetStatus()
	var cond *cloudv1.Condition
	for _, c := range st.GetConditions() {
		if c.GetType() == reconcilecluster.ConditionSpecEditIgnored {
			cond = c
		}
	}
	if cond == nil || cond.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE ||
		!strings.Contains(cond.GetMessage(), "ignored while imported") || !strings.Contains(st.GetMessage(), "ignored while imported") {
		t.Errorf("status = %q with condition %v, want the ignored edit reported", st.GetMessage(), cond)
	}
	if st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_IMPORTED {
		t.Errorf("state = %s, want IMPORTED still", st.GetState())
	}
	// A pass with nothing edited changes nothing more.
	before := storedCluster(t, w, c.GetId()).GetEtag()
	reconcile(t, w, c.GetId())
	if after := storedCluster(t, w, c.GetId()).GetEtag(); after != before {
		t.Error("a pass without an edit wrote the cluster again")
	}
}

// A takeover applies the plan that was reviewed: an edit made behind the
// API's back just before it, which no pass has seen yet, is not part of
// it.
func TestTakeoverAppliesTheReviewedPlanNotAnEdit(t *testing.T) {
	w := importWorld(t, terraformCluster())
	c := importTF(t, w)
	reconcile(t, w, c.GetId())
	discovered := storedCluster(t, w, c.GetId())

	editObject(t, w, c.GetId())
	res, err := w.svc.TakeOverCluster(t.Context(), &cloudv1.TakeOverClusterRequest{TenantId: worldTenant, ClusterId: c.GetId(),
		AcknowledgeNotImported: true})

	if err != nil {
		t.Fatalf("TakeOverCluster: %v", err)
	}
	if v, want := res.GetCluster().GetSpec().GetControlPlane().GetKubernetesVersion(), discovered.GetSpec().GetControlPlane().GetKubernetesVersion(); v != want {
		t.Errorf("taken over with kubernetes version %q, want the discovered %q", v, want)
	}
	if res.GetCluster().GetDisplayName() != discovered.GetDisplayName() {
		t.Errorf("taken over with display name %q, want %q", res.GetCluster().GetDisplayName(), discovered.GetDisplayName())
	}
}
