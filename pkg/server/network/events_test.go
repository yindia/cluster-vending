package network

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// eventStore is a store whose events are these: asked is what was asked
// for last.
type eventStore struct {
	repository.Store
	events []model.Event
	err    error
	asked  *[]string
}

func (s eventStore) Events() repository.Events { return s }

func (s eventStore) ListFor(_ context.Context, tenantID, kind, name string) ([]model.Event, error) {
	*s.asked = []string{tenantID, kind, name}
	return s.events, s.err
}

func TestListNetworkEvents(t *testing.T) {
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	events := []model.Event{
		{Time: at.Add(2 * time.Minute), Type: "Normal", Reason: "Ready", Count: 1, Kind: "Network", Name: "prod"},
		{Time: at.Add(time.Minute), Type: "Warning", Reason: "Failed", Message: "NATGateway n: quota", Count: 3, Kind: "Network", Name: "prod"},
		{Time: at, Type: "Normal", Reason: "Provisioning", Count: 1, Kind: "Network", Name: "prod"},
	}
	f := newFixture(t)
	n := f.create(t, "prod", nil)
	var asked []string
	svc := NewNetworkService(reconcile.Deps{Store: eventStore{Store: f.store, events: events, asked: &asked}, Kube: f.kube})
	tests := []struct {
		name     string
		req      *cloudv1.ListNetworkEventsRequest
		want     string
		wantNext string
		wantCode connect.Code
	}{
		{name: "everything, newest first", req: &cloudv1.ListNetworkEventsRequest{}, want: "Ready,Failed,Provisioning"},
		{name: "only the warnings", req: &cloudv1.ListNetworkEventsRequest{OnlyWarnings: true}, want: "Failed"},
		{name: "a page", req: &cloudv1.ListNetworkEventsRequest{PageSize: 1}, want: "Ready", wantNext: "1"},
		{name: "the next page", req: &cloudv1.ListNetworkEventsRequest{PageSize: 2, PageToken: "1"}, want: "Failed,Provisioning"},
		{name: "a token of another list", req: &cloudv1.ListNetworkEventsRequest{PageToken: "7"}, wantCode: connect.CodeInvalidArgument},
		{name: "a network that does not exist", req: &cloudv1.ListNetworkEventsRequest{NetworkId: unknownID}, wantCode: connect.CodeNotFound},
		{name: "another tenant", req: &cloudv1.ListNetworkEventsRequest{TenantId: "globex"}, wantCode: connect.CodeNotFound},
		{name: "no network ID", req: &cloudv1.ListNetworkEventsRequest{NetworkId: "nope"}, wantCode: connect.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.req.TenantId == "" {
				tt.req.TenantId = tenant
			}
			if tt.req.NetworkId == "" {
				tt.req.NetworkId = n.GetId()
			}

			res, err := svc.ListNetworkEvents(t.Context(), tt.req)

			if tt.wantCode != 0 {
				if connect.CodeOf(err) != tt.wantCode {
					t.Fatalf("error = %v, want %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListNetworkEvents: %v", err)
			}
			var have []string
			for _, e := range res.GetEvents() {
				have = append(have, e.GetReason())
			}
			if strings.Join(have, ",") != tt.want || res.GetNextPageToken() != tt.wantNext {
				t.Fatalf("events = %v next %q, want %s next %q", have, res.GetNextPageToken(), tt.want, tt.wantNext)
			}
			if strings.Join(asked, " ") != tenant+" Network prod" {
				t.Errorf("the store was asked for %v", asked)
			}
		})
	}
}

func TestListNetworkEventsWhenTheStoreFails(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil)
	var asked []string
	svc := NewNetworkService(reconcile.Deps{Store: eventStore{Store: f.store, err: errors.New("the API server is down"), asked: &asked}, Kube: f.kube})

	_, err := svc.ListNetworkEvents(t.Context(), &cloudv1.ListNetworkEventsRequest{TenantId: tenant, NetworkId: n.GetId()})

	if connect.CodeOf(err) != connect.CodeUnavailable || strings.Contains(err.Error(), "API server is down") {
		t.Fatalf("error = %v, want UNAVAILABLE without the store's text", err)
	}
}
