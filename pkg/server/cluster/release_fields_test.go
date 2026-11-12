package cluster

import (
	"reflect"
	"testing"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// The upgrade mode, the release channel and the tier are updatable. An
// update of only these stores them and changes no cloud resource: the
// generation counts as applied at once, the cluster's state stays, and a
// reconcile renders exactly the objects it had.
func TestReleaseFieldsAreUpdatableWithoutTouchingTheCloud(t *testing.T) {
	w := appliedWorld(t)
	ctx := t.Context()
	before := w.cluster(t)
	objects := objectsOf(t, w, w.row.ID)

	spec := &cloudv1.ClusterSpec{UpgradeMode: cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC,
		ReleaseChannel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, Tier: cloudv1.ClusterTier_CLUSTER_TIER_2}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, spec, pathUpgradeMode, pathReleaseChannel, pathTier)); err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
	after := w.cluster(t)
	if after.Generation != before.Generation+1 || after.AppliedGeneration != after.Generation || after.State != before.State {
		t.Errorf("generation %d -> %d, applied %d, state %s -> %s; want one more generation, applied, state kept",
			before.Generation, after.Generation, after.AppliedGeneration, before.State, after.State)
	}
	stored, err := reconcilecluster.SpecOf(after)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GetUpgradeMode() != spec.GetUpgradeMode() || stored.GetReleaseChannel() != spec.GetReleaseChannel() ||
		stored.GetTier() != spec.GetTier() {
		t.Errorf("stored mode %s, channel %s, tier %s", stored.GetUpgradeMode(), stored.GetReleaseChannel(), stored.GetTier())
	}

	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	now := objectsOf(t, w, w.row.ID)
	if len(now) != len(objects) {
		t.Fatalf("%d objects after the update, want the %d it had", len(now), len(objects))
	}
	for key, u := range objects {
		if !reflect.DeepEqual(u.Object["spec"], now[key].Object["spec"]) {
			t.Errorf("%s changed: an update of the release fields must not change a cloud resource", key)
		}
	}
}

// Unset values are stored as their defaults: manual mode, tier 0; an
// unset channel stays unset. An undefined value is refused.
func TestReleaseFieldDefaultsOnUpdate(t *testing.T) {
	w := appliedWorld(t)
	ctx := t.Context()
	set := &cloudv1.ClusterSpec{UpgradeMode: cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC,
		ReleaseChannel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA, Tier: cloudv1.ClusterTier_CLUSTER_TIER_1}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, set, pathUpgradeMode, pathReleaseChannel, pathTier)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, &cloudv1.ClusterSpec{}, pathUpgradeMode, pathReleaseChannel, pathTier)); err != nil {
		t.Fatal(err)
	}
	stored, err := reconcilecluster.SpecOf(w.cluster(t))
	if err != nil {
		t.Fatal(err)
	}
	if stored.GetUpgradeMode() != cloudv1.UpgradeMode_UPGRADE_MODE_MANUAL || stored.GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_0 ||
		stored.GetReleaseChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED {
		t.Errorf("unset values stored as mode %s, tier %s, channel %s; want MANUAL, TIER_0, unset",
			stored.GetUpgradeMode(), stored.GetTier(), stored.GetReleaseChannel())
	}

	bad := &cloudv1.ClusterSpec{UpgradeMode: cloudv1.UpgradeMode(99)}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, bad, pathUpgradeMode)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("undefined upgrade mode: %v, want INVALID_ARGUMENT", err)
	}
}
