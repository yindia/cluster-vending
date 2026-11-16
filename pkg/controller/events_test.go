package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
)

// The reconcilers' events become Kubernetes events of the object: with
// its UID when it exists, by name when it is gone, and never for a kind
// the controller does not know.
func TestEventsRecordOnTheObject(t *testing.T) {
	existing := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "prod", UID: types.UID("uid-1")}}
	tests := []struct {
		name string
		obj  platform.Object
		want string // what the fake recorder saw; "" for nothing
	}{
		{name: "an object that exists", obj: platform.Object{Kind: "Cluster", Namespace: "tenant-acme", Name: "prod"},
			want: "Warning Failed NodeGroup ng: 50% done"},
		{name: "an object that is gone", obj: platform.Object{Kind: "Cluster", Namespace: "tenant-acme", Name: "gone"},
			want: "Warning Failed NodeGroup ng: 50% done"},
		{name: "a subtenant", obj: platform.Object{Kind: platform.KindSubTenant, Namespace: "tenant-acme", Name: "allocation"},
			want: "Warning Failed NodeGroup ng: 50% done"},
		{name: "a cluster-scoped object", obj: platform.Object{Kind: "ReleaseChannel", Name: "alpha"},
			want: "Warning Failed NodeGroup ng: 50% done"},
		{name: "a kind the controller does not know", obj: platform.Object{Kind: "Secret", Namespace: "x", Name: "y"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(4)
			e := NewEvents(recorder, releaseClient(t, existing))

			// The message has a % in it: it is data, not a format.
			e.Event(t.Context(), tt.obj, platform.EventWarning, platform.ReasonFailed, "NodeGroup ng: 50% done")

			select {
			case got := <-recorder.Events:
				if got != tt.want {
					t.Fatalf("recorded %q, want %q", got, tt.want)
				}
			default:
				if tt.want != "" {
					t.Fatal("nothing was recorded")
				}
			}
		})
	}
}

func TestEventsWithoutARecorder(t *testing.T) {
	var none *Events
	none.Event(t.Context(), platform.Object{Kind: "Cluster", Name: "prod"}, platform.EventNormal, platform.ReasonRunning, "ready")
	NewEvents(nil, nil).Event(t.Context(), platform.Object{Kind: "Cluster", Name: "prod"}, platform.EventNormal, platform.ReasonRunning, "ready")
}

// The object an event is about carries its UID when the object exists:
// `kubectl describe` lists events by it.
func TestEventsLookTheObjectUp(t *testing.T) {
	existing := &cloudv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "prod", UID: types.UID("uid-7")}}
	seen := &seeingRecorder{}
	e := NewEvents(seen, releaseClient(t, existing))

	e.Event(t.Context(), platform.Object{Kind: "Network", Namespace: "tenant-acme", Name: "prod"}, platform.EventNormal, platform.ReasonReady, "ready")
	e.Event(t.Context(), platform.Object{Kind: "Network", Namespace: "tenant-acme", Name: "gone"}, platform.EventNormal, platform.ReasonDeleted, "gone")

	if len(seen.objects) != 2 || seen.objects[0].GetUID() != "uid-7" || seen.objects[1].GetUID() != "" ||
		seen.objects[1].GetName() != "gone" || seen.objects[1].GetNamespace() != "tenant-acme" {
		t.Fatalf("recorded on %+v", seen.objects)
	}
}

// seeingRecorder keeps the objects events were recorded on.
type seeingRecorder struct{ objects []client.Object }

func (r *seeingRecorder) Eventf(regarding, _ runtime.Object, _, _, _, _ string, _ ...any) {
	if obj, ok := regarding.(client.Object); ok {
		r.objects = append(r.objects, obj)
	}
}

func TestAccountEvent(t *testing.T) {
	cond := func(status metav1.ConditionStatus, message string) *metav1.Condition {
		return &metav1.Condition{Type: ConditionReady, Status: status, Message: message}
	}
	tests := []struct {
		name string
		was  *metav1.Condition
		is   metav1.Condition
		want string
	}{
		{name: "bootstrapped the first time", is: *cond(metav1.ConditionTrue, ""), want: "Normal Bootstrapped"},
		{name: "still ready", was: cond(metav1.ConditionTrue, ""), is: *cond(metav1.ConditionTrue, "")},
		{name: "it fails", was: cond(metav1.ConditionTrue, ""), is: *cond(metav1.ConditionFalse, "no credentials Secret"),
			want: "Warning ProviderConfigFailed"},
		{name: "it fails the same way", was: cond(metav1.ConditionFalse, "no credentials Secret"), is: *cond(metav1.ConditionFalse, "no credentials Secret")},
		{name: "it fails another way", was: cond(metav1.ConditionFalse, "no credentials Secret"), is: *cond(metav1.ConditionFalse, "the Secret has no key"),
			want: "Warning ProviderConfigFailed"},
		{name: "it recovers", was: cond(metav1.ConditionFalse, "no credentials Secret"), is: *cond(metav1.ConditionTrue, ""), want: "Normal Bootstrapped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := accountEvent(tt.was, tt.is)

			have := ""
			if got != nil {
				have = got.Type + " " + got.Reason
			}
			if have != tt.want {
				t.Fatalf("event = %q, want %q", have, tt.want)
			}
			if got != nil && got.Type == platform.EventWarning && !strings.Contains(got.Message, tt.is.Message) {
				t.Errorf("message %q lacks %q", got.Message, tt.is.Message)
			}
		})
	}
}

func TestReleaseEvent(t *testing.T) {
	cond := func(status metav1.ConditionStatus, message string) *metav1.Condition {
		return &metav1.Condition{Type: cloudv1alpha1.ReleaseConditionValid, Status: status, Message: message}
	}
	tests := []struct {
		name    string
		was, is *metav1.Condition
		want    string
	}{
		{name: "no condition"},
		{name: "valid the first time", is: cond(metav1.ConditionTrue, ""), want: "Normal ReleaseValid"},
		{name: "still valid", was: cond(metav1.ConditionTrue, ""), is: cond(metav1.ConditionTrue, "")},
		{name: "becomes invalid", was: cond(metav1.ConditionTrue, ""), is: cond(metav1.ConditionFalse, "version group istio"),
			want: "Warning ReleaseInvalid"},
		{name: "invalid the same way", was: cond(metav1.ConditionFalse, "x"), is: cond(metav1.ConditionFalse, "x")},
		{name: "invalid another way", was: cond(metav1.ConditionFalse, "x"), is: cond(metav1.ConditionFalse, "y"), want: "Warning ReleaseInvalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := releaseEvent(tt.was, tt.is)

			have := ""
			if got != nil {
				have = got.Type + " " + got.Reason
			}
			if have != tt.want {
				t.Fatalf("event = %q, want %q", have, tt.want)
			}
		})
	}
}

func pending(st cloudv1alpha1.ReleaseChannelStatus, release string) cloudv1alpha1.ReleaseChannelStatus {
	until := metav1.NewTime(time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC))
	st.Pending = &cloudv1alpha1.PendingRelease{Release: release, Until: until}
	return st
}

func TestChannelEvents(t *testing.T) {
	status := func(release string, offering metav1.ConditionStatus, message string) cloudv1alpha1.ReleaseChannelStatus {
		st := cloudv1alpha1.ReleaseChannelStatus{Release: release}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: cloudv1alpha1.ChannelConditionOffering, Status: offering,
			Reason: "X", Message: message})
		return st
	}
	tests := []struct {
		name    string
		was, is cloudv1alpha1.ReleaseChannelStatus
		want    []string
	}{
		{name: "the first release", is: status("r1", metav1.ConditionTrue, "offers r1"), want: []string{"Normal ChannelOffers"}},
		{name: "the same release", was: status("r1", metav1.ConditionTrue, "offers r1"), is: status("r1", metav1.ConditionTrue, "offers r1")},
		{name: "another release", was: status("r1", metav1.ConditionTrue, "offers r1"), is: status("r2", metav1.ConditionTrue, "offers r2"),
			want: []string{"Normal ChannelOffers"}},
		{name: "cannot move", was: status("r1", metav1.ConditionTrue, "offers r1"),
			is: status("r1", metav1.ConditionFalse, "release r2 is not valid; still offers r1"), want: []string{"Warning ChannelCannotMove"}},
		{name: "still cannot move", was: status("r1", metav1.ConditionFalse, "release r2 is not valid; still offers r1"),
			is: status("r1", metav1.ConditionFalse, "release r2 is not valid; still offers r1")},
		{name: "cannot move for another reason", was: status("r1", metav1.ConditionFalse, "release r2 is not valid; still offers r1"),
			is: status("r1", metav1.ConditionFalse, "release r2 is withdrawn; still offers r1"), want: []string{"Warning ChannelCannotMove"}},
		{name: "starts waiting out a release soak", was: status("r1", metav1.ConditionTrue, "offers r1"),
			is: pending(status("r1", metav1.ConditionTrue, "offers r1"), "r2"), want: []string{"Normal PromotionWaiting"}},
		{name: "still waiting", was: pending(status("r1", metav1.ConditionTrue, "offers r1"), "r2"),
			is: pending(status("r1", metav1.ConditionTrue, "offers r1"), "r2")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := channelEvents(tt.was, tt.is)

			var have []string
			for _, n := range got {
				have = append(have, n.Type+" "+n.Reason)
			}
			if !slices.Equal(have, tt.want) {
				t.Fatalf("events = %v, want %v", have, tt.want)
			}
		})
	}
}

// A release becoming valid and a channel offering it are events, once.
func TestReleaseControllersEmitEvents(t *testing.T) {
	rec := &platform.RecordedEvents{}
	alpha := DefaultChannels()[0]
	c := releaseClient(t, DefaultRelease(), alpha)
	releases := &ReleaseReconciler{c: c, render: DefaultRelease().Spec.Content.RenderVersion, events: rec}
	channels := &ReleaseChannelReconciler{c: c, events: rec}

	reconcileOnce(t, releases, DefaultReleaseName)
	reconcileOnce(t, channels, "alpha")

	if got := rec.Reasons(); !slices.Equal(got, []string{platform.ReasonReleaseValid, platform.ReasonChannelOffers}) {
		t.Fatalf("events = %v", got)
	}
	all := rec.All()
	if all[0].Object != (platform.Object{Kind: "Release", Name: DefaultReleaseName}) || all[1].Object != (platform.Object{Kind: "ReleaseChannel", Name: "alpha"}) {
		t.Errorf("objects = %+v, %+v", all[0].Object, all[1].Object)
	}

	rec.Reset()
	reconcileOnce(t, releases, DefaultReleaseName)
	reconcileOnce(t, channels, "alpha")
	if got := rec.Reasons(); len(got) != 0 {
		t.Errorf("a pass that changed nothing emitted %v", got)
	}
}
