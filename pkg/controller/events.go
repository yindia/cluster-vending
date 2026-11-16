package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
)

// The controllers report what happens as Kubernetes events (the
// events.k8s.io API) of the objects it happens to: in the object's
// namespace, and in "default" for the cluster-scoped Release and
// ReleaseChannel.
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// EventsName is who the controller's events say they are from
// (reportingController).
const EventsName = "cluster-vending-controller"

// eventAction is the action of every event: what the controller was doing
// when it happened.
const eventAction = "Reconcile"

// Events reports what the reconcilers say (platform.Events) through the
// manager's event recorder.
type Events struct {
	recorder events.EventRecorder
	// reader finds the object an event is about, for its UID: without it
	// `kubectl describe` does not list the event with the object.
	reader client.Reader
}

// NewEvents returns the Events that record with recorder
// (mgr.GetEventRecorder) and look objects up with reader.
func NewEvents(recorder events.EventRecorder, reader client.Reader) *Events {
	return &Events{recorder: recorder, reader: reader}
}

// Event implements platform.Events. An object that is gone (a deleted
// cluster's last event) is referred to by its name alone; a kind the
// controller does not know is dropped.
func (e *Events) Event(ctx context.Context, obj platform.Object, eventType, reason, message string) {
	if e == nil || e.recorder == nil {
		return
	}
	regarding := objectOf(obj)
	if regarding == nil {
		return
	}
	if e.reader != nil {
		found := objectOf(obj)
		if err := e.reader.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Name}, found); err == nil {
			regarding = found
		}
	}
	// The note is a format string to the recorder: the message is data.
	e.recorder.Eventf(regarding, nil, eventType, reason, eventAction, "%s", message)
}

// objectOf is an empty object of obj's kind with its name.
func objectOf(obj platform.Object) client.Object {
	meta := metav1.ObjectMeta{Namespace: obj.Namespace, Name: obj.Name}
	switch obj.Kind {
	case platform.KindCloudAccount:
		return &cloudv1alpha1.CloudAccount{ObjectMeta: meta}
	case platform.KindNetwork:
		return &cloudv1alpha1.Network{ObjectMeta: meta}
	case platform.KindCluster:
		return &cloudv1alpha1.Cluster{ObjectMeta: meta}
	case platform.KindSubTenant:
		return &cloudv1alpha1.SubTenant{ObjectMeta: meta}
	case platform.KindRelease:
		return &cloudv1alpha1.Release{ObjectMeta: meta}
	case platform.KindReleaseChannel:
		return &cloudv1alpha1.ReleaseChannel{ObjectMeta: meta}
	default:
		return nil
	}
}
