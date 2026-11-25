package cluster

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// haltWorld is the server's world with the controller running against the
// same store and management cluster, and providers that report what the
// objects ask for at once: the add-ons fail while failing is set.
type haltWorld struct {
	world
	ctl     *reconcilecluster.Reconciler
	failing bool
}

// pass is one pass of the controller, then one look of the providers.
func (h *haltWorld) pass(t *testing.T) {
	t.Helper()
	if err := h.ctl.Reconcile(t.Context(), worldTenant, h.row.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	h.look(t)
}

// look plays the providers: every recorded object reports the version it
// asks for and is Ready, but a failing add-on, whose update AWS fails.
func (h *haltWorld) look(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	records, err := h.store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, h.row.ID)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	for _, r := range records {
		u := h.object(t, r)
		if u == nil {
			continue // not applied (yet)
		}
		field := map[string]string{"Cluster": "version", "NodeGroup": "version", "Addon": "addonVersion"}[r.Kind]
		if field != "" {
			v, _, _ := unstructured.NestedString(u.Object, "spec", "forProvider", field)
			_ = unstructured.SetNestedField(u.Object, v, "status", "atProvider", field)
		}
		ready, synced := condition("Ready", "True", "Available", "", u, stamp), condition("Synced", "True", "ReconcileSuccess", "", u, stamp)
		if r.Kind == "Addon" && h.failing {
			ready = condition("Ready", "False", "UPDATE_FAILED", "", u, stamp)
			synced = condition("Synced", "False", "ReconcileError", "update failed: DEGRADED, pods not ready", u, stamp)
		}
		_ = unstructured.SetNestedSlice(u.Object, []any{ready, synced}, "status", "conditions")
		if err := h.kube.Update(ctx, u); err != nil {
			t.Fatalf("%s %s: %v", r.Kind, r.Name, err)
		}
	}
}

func condition(kind, status, reason, message string, u *unstructured.Unstructured, at string) map[string]any {
	c := map[string]any{"type": kind, "status": status, "reason": reason, "observedGeneration": u.GetGeneration(), "lastTransitionTime": at}
	if message != "" {
		c["message"] = message
	}
	return c
}

func (h *haltWorld) object(t *testing.T, r model.ManagedResource) *unstructured.Unstructured {
	t.Helper()
	gv, err := schema.ParseGroupVersion(r.APIVersion)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gv.WithKind(r.Kind))
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, u); err != nil {
		return nil
	}
	return u
}

// addons are the cluster's Addon objects as the management cluster has them.
func (h *haltWorld) addons(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	records, err := h.store.ManagedResources().ListByOwner(t.Context(), model.OwnerCluster, h.row.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []*unstructured.Unstructured
	for _, r := range records {
		if u := h.object(t, r); r.Kind == "Addon" && u != nil {
			out = append(out, u)
		}
	}
	return out
}

func (h *haltWorld) status(t *testing.T) *cloudv1.ClusterStatus {
	t.Helper()
	st := &cloudv1.ClusterStatus{}
	if err := reconcilecluster.Lenient.Unmarshal(h.cluster(t).Status, st); err != nil {
		t.Fatal(err)
	}
	return st
}

// setVersion is an update of the cluster to version, with the add-on
// versions EKS recommends for it (what resolve stores).
func (h *haltWorld) setVersion(t *testing.T, version, addonVersion string) {
	t.Helper()
	row := h.cluster(t)
	spec, err := reconcilecluster.SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.ControlPlane.KubernetesVersion = version
	for _, a := range spec.GetAws().GetAddons() {
		a.Version = addonVersion
	}
	if row.Spec, err = reconcilecluster.Encode(spec); err != nil {
		t.Fatal(err)
	}
	row.Generation++
	row.State = reconcilecluster.StateUpdating
	row.ResolvedGeneration = row.Generation
	if err := h.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
}

// A manual upgrade whose add-ons halted it stays halted pass after pass;
// cluster retry, through RetryCluster and the controller's next pass,
// clears the halt, the add-ons are recovered again (their watch starts
// over), and once they settle the upgrade finishes.
func TestRetriedHaltedManualUpgradeResumesInTheController(t *testing.T) {
	h := &haltWorld{world: newWorld(t, interceptor.Funcs{}, nil)}
	h.ctl = reconcilecluster.New(recon.Deps{Store: h.store, Kube: h.kube})
	ctx := t.Context()

	// The cluster runs 1.35, settled.
	h.setVersion(t, "1.35", "v1.35-eksbuild.1")
	for range 4 {
		h.pass(t)
	}
	if u := h.status(t).GetUpgrade(); u != nil {
		t.Fatalf("an upgrade before any was asked for: %v", u)
	}

	// The upgrade to 1.36 reaches its add-ons, whose update AWS fails.
	h.setVersion(t, "1.36", "v1.36-eksbuild.1")
	for pass := 0; ; pass++ {
		if pass == 30 {
			t.Fatalf("the upgrade never applied its add-ons: %v", h.status(t).GetUpgrade())
		}
		u := h.status(t).GetUpgrade()
		if u.GetPhase() == cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_ADDONS && u.GetApplied() {
			break
		}
		h.failing = u.GetPhase() == cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_ADDONS
		h.pass(t)
	}
	h.failing = true
	h.look(t)

	// It halted after its recoveries (as the add-ons' watch leaves it).
	row := h.cluster(t)
	st := h.status(t)
	st.Upgrade.Halted, st.Upgrade.Recoveries = true, 3
	st.Upgrade.Message = "halted: the add-ons did not settle after 3 recoveries"
	var err error
	if row.Status, err = reconcilecluster.Encode(st); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Clusters().Record(ctx, row); err != nil {
		t.Fatal(err)
	}
	forgetNudges := func() {
		for _, a := range h.addons(t) {
			ann := a.GetAnnotations()
			delete(ann, reconcilecluster.AnnotationRetriedAt)
			a.SetAnnotations(ann)
			if err := h.kube.Update(ctx, a); err != nil {
				t.Fatal(err)
			}
		}
	}
	nudged := func() bool {
		for _, a := range h.addons(t) {
			if a.GetAnnotations()[reconcilecluster.AnnotationRetriedAt] != "" {
				return true
			}
		}
		return false
	}
	forgetNudges()

	// Halted, it stays so: nothing recovers the add-ons.
	h.pass(t)
	if u := h.status(t).GetUpgrade(); !u.GetHalted() || u.GetRecoveries() != 3 || nudged() {
		t.Fatalf("without a retry the upgrade = %v (nudged %v), want still halted", u, nudged())
	}

	res, err := h.svc.RetryCluster(ctx, &cloudv1.RetryClusterRequest{TenantId: worldTenant, ClusterId: h.row.ID})
	if err != nil {
		t.Fatal(err)
	}
	if up := byName(res.GetItems())[upgradeItem]; up.GetAction() != cloudv1.RetryAction_RETRY_ACTION_RETRY {
		t.Fatalf("upgrade item = %v, want RETRY", up)
	}

	// The controller's pass acts on the request: the halt is cleared, the
	// watch starts over, and the request is done.
	h.pass(t)
	if r := h.cluster(t).Retry; r != nil {
		t.Errorf("retry = %+v after the pass, want it cleared", r)
	}
	u := h.status(t).GetUpgrade()
	if u.GetHalted() || u.GetRecoveries() != 0 || u.GetPhase() != cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_ADDONS {
		t.Fatalf("after the retried pass the upgrade = %v, want the add-ons' step, not halted, recoveries 0", u)
	}

	// The next pass recovers the still failing add-ons again.
	h.pass(t)
	if u := h.status(t).GetUpgrade(); u.GetHalted() || u.GetRecoveries() != 1 || !nudged() {
		t.Fatalf("the resumed upgrade = %v (nudged %v), want the add-ons recovered (attempt 1)", u, nudged())
	}

	// The add-ons settle: the upgrade goes on and finishes.
	h.failing = false
	for pass := 0; h.status(t).GetUpgrade() != nil; pass++ {
		if pass == 30 {
			t.Fatalf("the resumed upgrade never finished: %v", h.status(t).GetUpgrade())
		}
		h.pass(t)
	}
	if got := h.status(t).GetCurrentKubernetesVersion(); got != "1.36" {
		t.Errorf("after the upgrade the cluster reports %q, want 1.36", got)
	}
}
