package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// maxEventPages bounds how many pages one command reads.
const maxEventPages = 20

// eventsPage is one page of an object's events.
type eventsPage interface {
	proto.Message
	GetEvents() []*cloudv1.Event
	GetNextPageToken() string
}

// ClusterEvents calls ClusterService.ListClusterEvents for every page:
// what the controller reported about the cluster, newest first. With
// --output table it prints a table itself; else the result is printed as
// YAML or JSON.
func ClusterEvents(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	events, next, err := allEvents(o, func(token string) (eventsPage, error) {
		res, err := call(ctx, &cloudv1.ListClusterEventsRequest{TenantId: o.Tenant, ClusterId: o.ID,
			PageSize: o.PageSize, PageToken: token, OnlyWarnings: o.OnlyWarnings}, c.Cluster.ListClusterEvents)
		if err != nil {
			return nil, err
		}
		return res.(*cloudv1.ListClusterEventsResponse), nil
	})
	if err != nil {
		return nil, err
	}
	if o.Output != OutputTable {
		return &cloudv1.ListClusterEventsResponse{Events: events, NextPageToken: next}, nil
	}
	return nil, printEvents(o.Out, events, "cluster")
}

// NetworkEvents is ClusterEvents for a network
// (NetworkService.ListNetworkEvents).
func NetworkEvents(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	events, next, err := allEvents(o, func(token string) (eventsPage, error) {
		res, err := call(ctx, &cloudv1.ListNetworkEventsRequest{TenantId: o.Tenant, NetworkId: o.ID,
			PageSize: o.PageSize, PageToken: token, OnlyWarnings: o.OnlyWarnings}, c.Network.ListNetworkEvents)
		if err != nil {
			return nil, err
		}
		return res.(*cloudv1.ListNetworkEventsResponse), nil
	})
	if err != nil {
		return nil, err
	}
	if o.Output != OutputTable {
		return &cloudv1.ListNetworkEventsResponse{Events: events, NextPageToken: next}, nil
	}
	return nil, printEvents(o.Out, events, "network")
}

// allEvents reads every page with list; one page when --page-size asks
// for one, with the token of the next.
func allEvents(o Options, list func(token string) (eventsPage, error)) (events []*cloudv1.Event, next string, err error) {
	token := o.PageToken
	for range maxEventPages {
		page, err := list(token)
		if err != nil {
			return nil, "", err
		}
		events = append(events, page.GetEvents()...)
		if token = page.GetNextPageToken(); token == "" || o.PageSize > 0 {
			return events, token, nil
		}
	}
	return events, token, nil
}

// printEvents writes events as a table, newest first: when, how often,
// the type, the reason and the message on one line.
func printEvents(w io.Writer, events []*cloudv1.Event, noun string) error {
	if len(events) == 0 {
		_, err := fmt.Fprintf(w, "No events: Kubernetes keeps them for about an hour. The %s's status.recentEvents keeps the last ten (get).\n", noun)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "TIME\tCOUNT\tTYPE\tREASON\tMESSAGE"); err != nil {
		return err
	}
	for _, e := range events {
		kind := "Normal"
		if e.GetType() == cloudv1.EventType_EVENT_TYPE_WARNING {
			kind = "Warning"
		}
		when := "-"
		if e.GetTime() != nil {
			when = e.GetTime().AsTime().UTC().Format(time.RFC3339)
		}
		message := strings.Join(strings.Fields(e.GetMessage()), " ") // one line
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", when, max(e.GetCount(), 1), kind, e.GetReason(), message); err != nil {
			return err
		}
	}
	return tw.Flush()
}
