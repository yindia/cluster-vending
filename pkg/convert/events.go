package convert

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Event types as the kinds spell them (Kubernetes' own words).
const (
	eventNormal  = "Normal"
	eventWarning = "Warning"
)

// maxEventReason is the longest reason a RecentEvent may have.
const maxEventReason = 64

// recentEvents are the contract's recent events as the kind lists them:
// the last MaxRecentEvents, oldest first; nil for none. An event without a
// time or a reason is left out (the kind requires both).
func recentEvents(in []*cloudv1.Event) []cloudv1alpha1.RecentEvent {
	if len(in) > cloudv1alpha1.MaxRecentEvents {
		in = in[len(in)-cloudv1alpha1.MaxRecentEvents:]
	}
	var out []cloudv1alpha1.RecentEvent
	for _, e := range in {
		if e.GetTime() == nil || e.GetReason() == "" {
			continue
		}
		kind := eventNormal
		if e.GetType() == cloudv1.EventType_EVENT_TYPE_WARNING {
			kind = eventWarning
		}
		reason := e.GetReason()
		if len(reason) > maxEventReason {
			reason = reason[:maxEventReason]
		}
		// To the second, as the API server stores a metav1.Time: a time read
		// back must equal the one written, or every pass writes the status.
		out = append(out, cloudv1alpha1.RecentEvent{Time: metav1.NewTime(e.GetTime().AsTime().Truncate(time.Second)), Type: kind,
			Reason: reason, Message: e.GetMessage(), Count: e.GetCount()})
	}
	return out
}
