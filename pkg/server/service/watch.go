package service

import (
	"context"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// WatchInterval is how often a watch reads its resource.
const WatchInterval = 5 * time.Second

// watchRetries is how many failed reads in a row a watch tolerates before
// it fails (e.g. the management cluster is briefly unreachable).
const watchRetries = 3

// Snapshot is one read of a watched network or cluster.
type Snapshot[T proto.Message] struct {
	// Resource is the network or cluster.
	Resource T
	// Resources are its managed resources (status.managed_resources).
	Resources []*cloudv1.ManagedResource
	// Summary changes when the resource itself should be sent again (state,
	// message, generation).
	Summary string
}

// Message is one message of a watch stream.
type Message[T proto.Message] struct {
	// Resource is set in the first message and when Summary changed.
	Resource T
	Changed  bool
	Events   []*cloudv1.ManagedResourceEvent
	Deleted  bool
}

// Watch reads the resource every interval and sends what changed until it
// is gone (a Deleted message), ctx ends, or reads keep failing. A read
// returning NOT_FOUND first fails the watch (the resource does not exist);
// later it ends it as deleted.
func Watch[T proto.Message](ctx context.Context, interval time.Duration,
	read func(context.Context) (Snapshot[T], error), send func(Message[T]) error,
) error {
	tracker := &resourceTracker{}
	lastSummary := ""
	first, failures := true, 0
	for {
		snap, err := read(ctx)
		switch {
		case err == nil:
			failures = 0
			msg := Message[T]{Events: tracker.diff(snap.Resources)}
			if first || snap.Summary != lastSummary {
				msg.Resource, msg.Changed = snap.Resource, true
				lastSummary = snap.Summary
			}
			if first || msg.Changed || len(msg.Events) > 0 {
				if err := send(msg); err != nil {
					return err
				}
			}
			first = false
		case connect.CodeOf(err) == connect.CodeNotFound && !first:
			return send(Message[T]{Events: tracker.diff(nil), Deleted: true})
		case connect.CodeOf(err) == connect.CodeNotFound, ctx.Err() != nil:
			return err
		default:
			failures++
			if failures > watchRetries {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// resourceTracker remembers the last observed managed resources to turn
// reads into events.
type resourceTracker struct {
	last  map[string]*cloudv1.ManagedResource
	order []string
}

func (t *resourceTracker) diff(views []*cloudv1.ManagedResource) []*cloudv1.ManagedResourceEvent {
	var events []*cloudv1.ManagedResourceEvent
	now := make(map[string]*cloudv1.ManagedResource, len(views))
	order := make([]string, 0, len(views))
	for _, v := range views {
		key := v.GetKind() + "/" + v.GetName()
		now[key] = v
		order = append(order, key)
		prev, seen := t.last[key]
		switch {
		case !seen:
			events = append(events, event(cloudv1.ManagedResourceEvent_TYPE_ADDED, v))
		case !proto.Equal(prev, v):
			events = append(events, event(cloudv1.ManagedResourceEvent_TYPE_MODIFIED, v))
		}
	}
	for _, key := range t.order {
		if _, ok := now[key]; !ok {
			events = append(events, event(cloudv1.ManagedResourceEvent_TYPE_DELETED, t.last[key]))
		}
	}
	t.last, t.order = now, order
	return events
}

func event(typ cloudv1.ManagedResourceEvent_Type, r *cloudv1.ManagedResource) *cloudv1.ManagedResourceEvent {
	return &cloudv1.ManagedResourceEvent{Type: typ, Resource: r}
}
