package controller

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// An Automatic channel takes the release its upstream offers only once
// the upstream has offered it for the channel's release soak; an admin's
// promotion takes it at once. A channel offering nothing yet takes it at
// once too (a new management cluster).
func TestChannelWaitsOutTheReleaseSoak(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	valid := metav1.Condition{Type: cloudv1alpha1.ReleaseConditionValid, Status: metav1.ConditionTrue, Reason: "Valid"}
	older := DefaultRelease()
	meta.SetStatusCondition(&older.Status.Conditions, valid)
	newer := DefaultRelease()
	newer.Name = "2026-11-a"
	newer.Status = older.Status
	alpha := DefaultChannels()[0]
	alpha.Spec.Release = newer.Name
	alpha.Status.Release = newer.Name
	alpha.Status.History = []cloudv1alpha1.ChannelHistoryEntry{{Release: newer.Name, From: metav1.NewTime(start)}}
	beta := DefaultChannels()[1]
	beta.Status.Release = older.Name
	beta.Status.History = []cloudv1alpha1.ChannelHistoryEntry{{Release: older.Name, From: metav1.NewTime(start.Add(-30 * 24 * time.Hour))}}

	tests := []struct {
		name    string
		after   time.Duration
		soak    *metav1.Duration
		want    string
		waiting bool
	}{
		{name: "within the default soak (7d for beta)", after: 24 * time.Hour, want: older.Name, waiting: true},
		{name: "after it", after: 8 * 24 * time.Hour, want: newer.Name},
		{name: "a soak of its own", after: 2 * 24 * time.Hour, soak: &metav1.Duration{Duration: 24 * time.Hour}, want: newer.Name},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := beta.DeepCopy()
			ch.Spec.Policy.ReleaseSoak = tt.soak
			c := releaseClient(t, older.DeepCopy(), newer.DeepCopy(), alpha.DeepCopy(), ch)
			now := start.Add(tt.after)
			r := &ReleaseChannelReconciler{c: c, now: func() time.Time { return now }}

			res := reconcileOnce(t, r, "beta")

			got := &cloudv1alpha1.ReleaseChannel{}
			if err := c.Get(t.Context(), client.ObjectKey{Name: "beta"}, got); err != nil {
				t.Fatal(err)
			}
			if got.Status.Release != tt.want {
				t.Fatalf("beta offers %s, want %s", got.Status.Release, tt.want)
			}
			if tt.waiting {
				p := got.Status.Pending
				if p == nil || p.Release != newer.Name || !p.Until.Time.Equal(start.Add(7*24*time.Hour)) {
					t.Errorf("pending = %+v, want %s until %s", p, newer.Name, start.Add(7*24*time.Hour))
				}
				if want := start.Add(7 * 24 * time.Hour).Sub(now); res.RequeueAfter != want {
					t.Errorf("requeue after %s, want %s (the soak's end)", res.RequeueAfter, want)
				}
				cond := meta.FindStatusCondition(got.Status.Conditions, cloudv1alpha1.ChannelConditionOffering)
				if cond == nil || !strings.Contains(cond.Message, "waiting to take "+newer.Name) {
					t.Errorf("Offering = %+v, want it to say what it waits for", cond)
				}
				return
			}
			if got.Status.Pending != nil {
				t.Errorf("pending = %+v, want none", got.Status.Pending)
			}
		})
	}
}
