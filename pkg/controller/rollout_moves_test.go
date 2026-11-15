package controller

import (
	"slices"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// The channel's status also says which clusters may start moving their
// components to its release.
func TestRolloutPlansReleaseMoves(t *testing.T) {
	old := rolloutCluster("old", cloudv1alpha1.ClusterTier2, "1.36")
	old.Status.PlatformRelease = "shipped-00000000"
	on := rolloutCluster("on", cloudv1alpha1.ClusterTier2, "1.36")
	on.Status.PlatformRelease = "shipped-00000001"
	c := rolloutWorld(t, defaultPolicy, old, on)

	plan := func() cloudv1alpha1.ReleaseChannelStatus {
		r := &RolloutReconciler{c: c, now: func() time.Time { return rolloutNow }, events: &channelEventsSeen{}}
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "stable"}}); err != nil {
			t.Fatal(err)
		}
		ch := &cloudv1alpha1.ReleaseChannel{}
		if err := c.Get(t.Context(), client.ObjectKey{Name: "stable"}, ch); err != nil {
			t.Fatal(err)
		}
		return ch.Status
	}

	st := plan()
	if m := st.ReleaseMoves; m == nil || m.Release != "shipped-00000001" || !slices.Equal(m.Admitted, []string{"tenant-acme/old"}) {
		t.Fatalf("releaseMoves = %+v", st.ReleaseMoves)
	}
}
