package cli

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

const eventsObject = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"

func someEvents() []*cloudv1.Event {
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	return []*cloudv1.Event{
		{Time: timestamppb.New(at.Add(2 * time.Minute)), Type: cloudv1.EventType_EVENT_TYPE_WARNING, Reason: "Failed",
			Message: "NodeGroup ng:\n  quota exceeded", Count: 3},
		{Time: timestamppb.New(at.Add(time.Minute)), Type: cloudv1.EventType_EVENT_TYPE_NORMAL, Reason: "ControlPlaneActive",
			Message: "the EKS cluster is ACTIVE", Count: 1},
		{Time: timestamppb.New(at), Type: cloudv1.EventType_EVENT_TYPE_NORMAL, Reason: "Provisioning", Message: "creating the cluster's resources"},
	}
}

// page is what a fake service answers: two events a page unless the
// request asks otherwise, only the warnings when asked.
func page(events []*cloudv1.Event, size int32, token string, onlyWarnings bool) ([]*cloudv1.Event, string) {
	if onlyWarnings {
		var warnings []*cloudv1.Event
		for _, e := range events {
			if e.GetType() == cloudv1.EventType_EVENT_TYPE_WARNING {
				warnings = append(warnings, e)
			}
		}
		events = warnings
	}
	start, _ := strconv.Atoi(token)
	n := 2
	if size > 0 {
		n = int(size)
	}
	end := min(start+n, len(events))
	next := ""
	if end < len(events) {
		next = strconv.Itoa(end)
	}
	return events[start:end], next
}

type clusterEvents struct {
	cloudv1connect.ClusterServiceClient
	events   []*cloudv1.Event
	err      error
	requests []*cloudv1.ListClusterEventsRequest
}

func (s *clusterEvents) ListClusterEvents(_ context.Context, req *cloudv1.ListClusterEventsRequest) (*cloudv1.ListClusterEventsResponse, error) {
	s.requests = append(s.requests, req)
	if s.err != nil {
		return nil, s.err
	}
	events, next := page(s.events, req.GetPageSize(), req.GetPageToken(), req.GetOnlyWarnings())
	return &cloudv1.ListClusterEventsResponse{Events: events, NextPageToken: next}, nil
}

type networkEvents struct {
	cloudv1connect.NetworkServiceClient
	events   []*cloudv1.Event
	requests []*cloudv1.ListNetworkEventsRequest
}

func (s *networkEvents) ListNetworkEvents(_ context.Context, req *cloudv1.ListNetworkEventsRequest) (*cloudv1.ListNetworkEventsResponse, error) {
	s.requests = append(s.requests, req)
	events, next := page(s.events, req.GetPageSize(), req.GetPageToken(), req.GetOnlyWarnings())
	return &cloudv1.ListNetworkEventsResponse{Events: events, NextPageToken: next}, nil
}

func TestClusterEventsTable(t *testing.T) {
	withClock(t)
	tests := []struct {
		name         string
		events       []*cloudv1.Event
		opts         Options
		want         []string // lines
		wantRequests int
	}{
		{name: "every page, newest first", events: someEvents(), opts: Options{Tenant: "acme", ID: eventsObject}, wantRequests: 2,
			want: []string{
				"LAST SEEN   COUNT   TYPE      REASON               MESSAGE",
				"25h         3       Warning   Failed               NodeGroup ng: quota exceeded",
				"25h         1       Normal    ControlPlaneActive   the EKS cluster is ACTIVE",
				"26h         1       Normal    Provisioning         creating the cluster's resources",
			}},
		{name: "only the warnings", events: someEvents(), opts: Options{Tenant: "acme", ID: eventsObject, OnlyWarnings: true}, wantRequests: 1,
			want: []string{
				"LAST SEEN   COUNT   TYPE      REASON   MESSAGE",
				"25h         3       Warning   Failed   NodeGroup ng: quota exceeded",
			}},
		{name: "no events", opts: Options{Tenant: "acme", ID: eventsObject}, wantRequests: 1,
			want: []string{"No events: Kubernetes keeps them for about an hour. The cluster's status.recentEvents keeps the last ten (get)."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &clusterEvents{events: tt.events}
			var out bytes.Buffer

			err := Run(t.Context(), ClusterEvents, Clients{Cluster: svc}, tt.opts, &out, OutputTable)

			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			var lines []string
			for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
				lines = append(lines, strings.TrimRight(l, " "))
			}
			if strings.Join(lines, "\n") != strings.Join(tt.want, "\n") {
				t.Fatalf("printed\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(tt.want, "\n"))
			}
			if len(svc.requests) != tt.wantRequests || svc.requests[0].GetTenantId() != "acme" || svc.requests[0].GetClusterId() != eventsObject {
				t.Errorf("requests = %v", svc.requests)
			}
		})
	}
}

// As data, one page asked for is one page returned, with the token of the
// next.
func TestClusterEventsAsData(t *testing.T) {
	svc := &clusterEvents{events: someEvents()}
	var out bytes.Buffer

	err := Run(t.Context(), ClusterEvents, Clients{Cluster: svc}, Options{Tenant: "acme", ID: eventsObject, PageSize: 1}, &out, OutputJSON)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.Join(strings.Fields(out.String()), "") // protojson varies its whitespace
	for _, want := range []string{`"reason":"Failed"`, `"type":"EVENT_TYPE_WARNING"`, `"nextPageToken":"1"`} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %s:\n%s", want, out.String())
		}
	}
	if len(svc.requests) != 1 || strings.Contains(got, "Provisioning") {
		t.Errorf("more than the page asked for: %d requests", len(svc.requests))
	}
}

func TestNetworkEvents(t *testing.T) {
	svc := &networkEvents{events: someEvents()[1:]}
	var out bytes.Buffer

	err := Run(t.Context(), NetworkEvents, Clients{Network: svc}, Options{Tenant: "acme", ID: eventsObject}, &out, OutputTable)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "ControlPlaneActive") || !strings.Contains(out.String(), "Provisioning") {
		t.Fatalf("printed %s", out.String())
	}
	if len(svc.requests) != 1 || svc.requests[0].GetNetworkId() != eventsObject {
		t.Errorf("requests = %v", svc.requests)
	}

	out.Reset()
	if err := Run(t.Context(), NetworkEvents, Clients{Network: &networkEvents{}}, Options{Tenant: "acme", ID: eventsObject}, &out, OutputTable); err != nil ||
		!strings.Contains(out.String(), "The network's status.recentEvents") {
		t.Fatalf("no events: %q, %v", out.String(), err)
	}
}

func TestEventsFailures(t *testing.T) {
	tests := []struct {
		name string
		cmd  Command
		c    Clients
		opts Options
		want string
	}{
		{"cluster events without a tenant", ClusterEvents, Clients{Cluster: &clusterEvents{}}, Options{ID: eventsObject}, "--tenant"},
		{"network events without a tenant", NetworkEvents, Clients{Network: &networkEvents{}}, Options{ID: eventsObject}, "--tenant"},
		{"the API fails", ClusterEvents, Clients{Cluster: &clusterEvents{err: errors.New("boom")}}, Options{Tenant: "acme", ID: eventsObject}, "boom"},
		{"not an ID", ClusterEvents, Clients{Cluster: &clusterEvents{}}, Options{Tenant: "acme", ID: "nope"}, "invalid request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			err := Run(t.Context(), tt.cmd, tt.c, tt.opts, &out, OutputTable)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run = %v, want an error naming %q", err, tt.want)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q with the error", out.String())
			}
		})
	}
}
