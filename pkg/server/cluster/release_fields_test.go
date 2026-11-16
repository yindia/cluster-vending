package cluster

import (
	"reflect"
	"testing"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// The upgrade mode, the release channel and the maintenance
// window are updatable. An
// update of only these stores them and changes no cloud resource: the
// generation counts as applied at once, the cluster's state stays, and a
// reconcile renders exactly the objects it had.
func TestReleaseFieldsAreUpdatableWithoutTouchingTheCloud(t *testing.T) {
	w := appliedWorld(t)
	ctx := t.Context()
	before := w.cluster(t)
	objects := objectsOf(t, w, w.row.ID)

	spec := &cloudv1.ClusterSpec{UpgradeMode: cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC,
		ReleaseChannel:    cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA,
		MaintenanceWindow: &cloudv1.MaintenanceWindow{Days: []cloudv1.Weekday{cloudv1.Weekday_WEEKDAY_SATURDAY}, Start: "02:00", Duration: "4h"}}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, spec, pathUpgradeMode, pathReleaseChannel, pathMaintenanceWindow)); err != nil {
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
		!proto.Equal(stored.GetMaintenanceWindow(), spec.GetMaintenanceWindow()) {
		t.Errorf("stored mode %s, channel %s, window %v", stored.GetUpgradeMode(), stored.GetReleaseChannel(),
			stored.GetMaintenanceWindow())
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

// Unset values are stored as their defaults: manual mode; an
// unset channel stays unset. An undefined value is refused.
func TestReleaseFieldDefaultsOnUpdate(t *testing.T) {
	w := appliedWorld(t)
	ctx := t.Context()
	set := &cloudv1.ClusterSpec{UpgradeMode: cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC,
		ReleaseChannel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, set, pathUpgradeMode, pathReleaseChannel)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, &cloudv1.ClusterSpec{}, pathUpgradeMode, pathReleaseChannel)); err != nil {
		t.Fatal(err)
	}
	stored, err := reconcilecluster.SpecOf(w.cluster(t))
	if err != nil {
		t.Fatal(err)
	}
	if stored.GetUpgradeMode() != cloudv1.UpgradeMode_UPGRADE_MODE_MANUAL ||
		stored.GetReleaseChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED {
		t.Errorf("unset values stored as mode %s, channel %s; want MANUAL, unset",
			stored.GetUpgradeMode(), stored.GetReleaseChannel())
	}

	// A window is removed by updating its path with none.
	window := &cloudv1.ClusterSpec{MaintenanceWindow: &cloudv1.MaintenanceWindow{Start: "02:00", Duration: "4h"}}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, window, pathMaintenanceWindow)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, &cloudv1.ClusterSpec{}, pathMaintenanceWindow)); err != nil {
		t.Fatal(err)
	}
	if stored, err := reconcilecluster.SpecOf(w.cluster(t)); err != nil || stored.GetMaintenanceWindow() != nil {
		t.Errorf("window after removing it: %v (%v), want none", stored.GetMaintenanceWindow(), err)
	}
	short := &cloudv1.ClusterSpec{MaintenanceWindow: &cloudv1.MaintenanceWindow{Start: "02:00", Duration: "10m"}}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, short, pathMaintenanceWindow)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("10m window: %v, want INVALID_ARGUMENT", err)
	}

	bad := &cloudv1.ClusterSpec{UpgradeMode: cloudv1.UpgradeMode(99)}
	if _, err := w.svc.UpdateCluster(ctx, updateRequest(w, bad, pathUpgradeMode)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("undefined upgrade mode: %v, want INVALID_ARGUMENT", err)
	}
}
