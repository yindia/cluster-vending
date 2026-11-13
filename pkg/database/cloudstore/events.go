package cloudstore

import (
	"context"
	"fmt"
	"sort"
	"time"

	eventsv1 "k8s.io/api/events/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func (s store) Events() repository.Events { return events{s} }

// events reads the Kubernetes events of a tenant's objects.
type events struct{ store }

// ListFor lists the events of the tenant's namespace and keeps those
// about the object: the namespace holds the events of one tenant's
// networks and clusters for about an hour, so it is small, and filtering
// here needs no field index. Newest first.
func (r events) ListFor(ctx context.Context, tenantID, kind, name string) ([]model.Event, error) {
	if !labelValue(tenantID) {
		return nil, nil
	}
	list := &eventsv1.EventList{}
	if err := r.c.List(ctx, list, client.InNamespace(kube.TenantNamespace(tenantID))); err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	var out []model.Event
	for i := range list.Items {
		e := &list.Items[i]
		if e.Regarding.Kind != kind || e.Regarding.Name != name {
			continue
		}
		out = append(out, model.Event{Time: eventTime(e), Type: e.Type, Reason: e.Reason, Message: e.Note,
			Count: eventCount(e), Kind: e.Regarding.Kind, Name: e.Regarding.Name})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// eventTime is when an event happened last: the last time of its series,
// the time it was recorded, or, for one written through the old events
// API, its last or first timestamp.
func eventTime(e *eventsv1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.DeprecatedLastTimestamp.IsZero():
		return e.DeprecatedLastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	case !e.DeprecatedFirstTimestamp.IsZero():
		return e.DeprecatedFirstTimestamp.Time
	default:
		return e.CreationTimestamp.Time
	}
}

// eventCount is how often an event happened: at least once.
func eventCount(e *eventsv1.Event) int32 {
	switch {
	case e.Series != nil && e.Series.Count > 0:
		return e.Series.Count
	case e.DeprecatedCount > 0:
		return e.DeprecatedCount
	default:
		return 1
	}
}
