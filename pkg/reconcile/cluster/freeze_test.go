package cluster

import (
	"context"
	"errors"
	"testing"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
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

// The platform freeze stops what the cluster reconciler starts on its own
// (the daily add-on upgrade, a code change); a cluster it lets through, or
// no freeze, does not stop it. Holds that cannot be read count as frozen:
// nothing automatic starts blind.
func TestReconcilerFrozen(t *testing.T) {
	freeze := cloudv1alpha1.ReleaseHold{Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "deploy", Except: []string{"let-through"}}}
	tests := []struct {
		name string
		src  freezeSource
		id   string
		want bool
	}{
		{"frozen", freezeSource{holds: []cloudv1alpha1.ReleaseHold{freeze}}, "c1", true},
		{"let through", freezeSource{holds: []cloudv1alpha1.ReleaseHold{freeze}}, "let-through", false},
		{"not frozen", freezeSource{}, "c1", false},
		{"holds unreadable", freezeSource{err: errors.New("down")}, "c1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(reconcile.Deps{Releases: tt.src})
			if got, _ := s.frozen(t.Context(), tt.id); got != tt.want {
				t.Errorf("frozen = %v, want %v", got, tt.want)
			}
		})
	}
}
