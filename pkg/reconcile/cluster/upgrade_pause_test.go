package cluster

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// holdSource is a release source that also has release holds.
type holdSource struct {
	channelSource
	holds []cloudv1alpha1.ReleaseHold
}

func (h holdSource) Holds(context.Context) ([]cloudv1alpha1.ReleaseHold, error) { return h.holds, nil }

// A cluster whose upgrades are paused, by a pause of its own, of its
// tenant or of its channel, says so in its status: why, until when, and
// what was paused. A pause whose until passed, or of another tenant, is
// not reported.
func TestStatusSaysTheUpgradesArePaused(t *testing.T) {
	until := metav1.NewTime(time.Now().Add(time.Hour))
	passed := metav1.NewTime(time.Now().Add(-time.Hour))
	tests := []struct {
		name      string
		spec      cloudv1alpha1.ReleaseHoldSpec
		wantScope string // "": not paused
	}{
		{"its tenant's", cloudv1alpha1.ReleaseHoldSpec{TenantID: worldTenant, Reason: "audit", Until: &until}, "tenant " + worldTenant},
		{"another tenant's", cloudv1alpha1.ReleaseHoldSpec{TenantID: "globex", Reason: "audit"}, ""},
		{"its tenant's, until passed", cloudv1alpha1.ReleaseHoldSpec{TenantID: worldTenant, Reason: "audit", Until: &passed}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := holdSource{channelSource: channelSource{rel: channelRelease()}, holds: []cloudv1alpha1.ReleaseHold{{Spec: tt.spec}}}
			w := newWorld(t, interceptor.Funcs{}, nil, func(d *reconcile.Deps) { d.Releases = src })
			if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
				t.Fatal(err)
			}

			st := &cloudv1.ClusterStatus{}
			if err := Lenient.Unmarshal(w.cluster(t).Status, st); err != nil {
				t.Fatal(err)
			}

			got := st.GetUpgradePause()
			switch {
			case tt.wantScope == "" && got != nil:
				t.Fatalf("upgrade_pause = %v, want none", got)
			case tt.wantScope != "" && (got.GetScope() != tt.wantScope || got.GetReason() != "audit" || got.GetUntil() == nil):
				t.Fatalf("upgrade_pause = %v, want scope %q", got, tt.wantScope)
			}
		})
	}
}
