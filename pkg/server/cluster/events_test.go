package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
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

func TestListClusterEvents(t *testing.T) {
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	events := []model.Event{
		{Time: at.Add(2 * time.Minute), Type: "Warning", Reason: "Failed", Message: "NodeGroup ng: x", Count: 2, Kind: "Cluster", Name: "prod"},
		{Time: at.Add(time.Minute), Type: "Normal", Reason: "ControlPlaneActive", Count: 1, Kind: "Cluster", Name: "prod"},
		{Time: at, Type: "Normal", Reason: "Provisioning", Count: 1, Kind: "Cluster", Name: "prod"},
	}
	var asked []string
	w := newWorld(t, interceptor.Funcs{}, nil, func(d *recon.Deps) {
		d.Store = eventStore{Store: d.Store, events: events, asked: &asked}
	})
	tests := []struct {
		name     string
		req      *cloudv1.ListClusterEventsRequest
		want     string
		wantNext string
		wantCode connect.Code
	}{
		{name: "everything, newest first", req: &cloudv1.ListClusterEventsRequest{}, want: "Failed,ControlPlaneActive,Provisioning"},
		{name: "only the warnings", req: &cloudv1.ListClusterEventsRequest{OnlyWarnings: true}, want: "Failed"},
		{name: "a page", req: &cloudv1.ListClusterEventsRequest{PageSize: 2}, want: "Failed,ControlPlaneActive", wantNext: "2"},
		{name: "the next page", req: &cloudv1.ListClusterEventsRequest{PageSize: 2, PageToken: "2"}, want: "Provisioning"},
		{name: "a token of another list", req: &cloudv1.ListClusterEventsRequest{PageToken: "7"}, wantCode: connect.CodeInvalidArgument},
		{name: "another tenant's cluster", req: &cloudv1.ListClusterEventsRequest{TenantId: "globex"}, wantCode: connect.CodeNotFound},
		{name: "no cluster ID", req: &cloudv1.ListClusterEventsRequest{ClusterId: "nope"}, wantCode: connect.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.req.TenantId == "" {
				tt.req.TenantId = worldTenant
			}
			if tt.req.ClusterId == "" {
				tt.req.ClusterId = w.row.ID
			}

			res, err := w.svc.ListClusterEvents(t.Context(), tt.req)

			if tt.wantCode != 0 {
				if connect.CodeOf(err) != tt.wantCode {
					t.Fatalf("error = %v, want %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListClusterEvents: %v", err)
			}
			var have []string
			for _, e := range res.GetEvents() {
				have = append(have, e.GetReason())
			}
			if strings.Join(have, ",") != tt.want || res.GetNextPageToken() != tt.wantNext {
				t.Fatalf("events = %v next %q, want %s next %q", have, res.GetNextPageToken(), tt.want, tt.wantNext)
			}
			// Asked for the cluster's object by its name, in its tenant.
			if strings.Join(asked, " ") != worldTenant+" Cluster "+w.row.Name {
				t.Errorf("the store was asked for %v", asked)
			}
		})
	}
}

func TestListClusterEventsWhenTheStoreFails(t *testing.T) {
	var asked []string
	w := newWorld(t, interceptor.Funcs{}, nil, func(d *recon.Deps) {
		d.Store = eventStore{Store: d.Store, err: errors.New("the API server is down"), asked: &asked}
	})

	_, err := w.svc.ListClusterEvents(t.Context(), &cloudv1.ListClusterEventsRequest{TenantId: worldTenant, ClusterId: w.row.ID})

	if err == nil || strings.Contains(err.Error(), "API server is down") {
		t.Fatalf("error = %v, want one that does not leak the store's text", err)
	}
}
