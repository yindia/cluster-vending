package cluster

import (
	"context"
	"errors"
	"testing"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// freezeSource is a release source whose holds may not be readable.
type freezeSource struct {
	reconcile.ReleaseReader
	holds []cloudv1alpha1.ReleaseHold
	err   error
}

func (h freezeSource) Holds(context.Context) ([]cloudv1alpha1.ReleaseHold, error) {
	return h.holds, h.err
}

// A freeze (of the platform, or of the cluster's tenant) stops what the
// cluster reconciler starts on its own (the daily add-on upgrade, a code
// change); a cluster it lets through, another tenant's freeze, or no
// freeze, does not stop it. Freezes that cannot be read count as frozen:
// nothing automatic starts blind.
func TestReconcilerFrozen(t *testing.T) {
	platform := cloudv1alpha1.ReleaseHold{Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "deploy", Except: []string{"let-through"}}}
	tests := []struct {
		name, id, tenant string
		src              freezeSource
		want             bool
		why              string
	}{
		{"frozen", "c1", "acme", freezeSource{holds: []cloudv1alpha1.ReleaseHold{platform}}, true, "the platform is frozen: deploy"},
		{"let through", "let-through", "acme", freezeSource{holds: []cloudv1alpha1.ReleaseHold{platform}}, false, ""},
		{"not frozen", "c1", "acme", freezeSource{}, false, ""},
		{"holds unreadable", "c1", "acme", freezeSource{err: errors.New("down")}, true, "cannot read whether the cluster is frozen; waiting"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(reconcile.Deps{Releases: tt.src})
			row := &model.Cluster{ID: tt.id, TenantID: tt.tenant}
			got, why := s.frozen(t.Context(), row, &cloudv1.ClusterSpec{})
			if got != tt.want || why != tt.why {
				t.Errorf("frozen = %v %q, want %v %q", got, why, tt.want, tt.why)
			}
		})
	}
}
