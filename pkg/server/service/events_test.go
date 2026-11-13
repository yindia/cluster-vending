package service_test

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func TestEventsPage(t *testing.T) {
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	events := []model.Event{
		{Time: at.Add(3 * time.Minute), Type: "Warning", Reason: "Failed", Message: "NodeGroup ng: x", Count: 3, Kind: "Cluster", Name: "prod"},
		{Time: at.Add(2 * time.Minute), Type: "Normal", Reason: "Updating", Kind: "Cluster", Name: "prod"},
		{Time: at.Add(time.Minute), Type: "Warning", Reason: "PlatformComponentFailed", Count: 1, Kind: "Cluster", Name: "prod"},
		{Time: at, Type: "Normal", Reason: "Provisioning", Count: 1, Kind: "Cluster", Name: "prod"},
	}
	tests := []struct {
		name         string
		onlyWarnings bool
		size         int32
		token        string
		want         string
		wantNext     string
		wantErr      bool
	}{
		{name: "everything", want: "Failed,Updating,PlatformComponentFailed,Provisioning"},
		{name: "only the warnings", onlyWarnings: true, want: "Failed,PlatformComponentFailed"},
		{name: "the first page", size: 3, want: "Failed,Updating,PlatformComponentFailed", wantNext: "3"},
		{name: "the next page", size: 3, token: "3", want: "Provisioning"},
		{name: "a page of warnings", onlyWarnings: true, size: 1, want: "Failed", wantNext: "1"},
		{name: "past the end", token: "4", want: ""},
		{name: "a token of another list", token: "9", wantErr: true},
		{name: "a token that is no number", token: "x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, next, err := service.EventsPage(events, tt.onlyWarnings, tt.size, tt.token)

			if tt.wantErr {
				if connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Fatalf("error = %v, want INVALID_ARGUMENT", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("EventsPage: %v", err)
			}
			var have []string
			for _, e := range page {
				have = append(have, e.GetReason())
			}
			if strings.Join(have, ",") != tt.want || next != tt.wantNext {
				t.Fatalf("page = %v next %q, want %s next %q", have, next, tt.want, tt.wantNext)
			}
		})
	}
}

func TestEventsPageAsTheContract(t *testing.T) {
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

	page, _, err := service.EventsPage([]model.Event{
		{Time: at, Type: "Warning", Reason: "Failed", Message: "NodeGroup ng: x", Count: 3, Kind: "Cluster", Name: "prod"},
		{Time: at, Type: "Normal", Reason: "Running", Kind: "Cluster", Name: "prod"},
	}, false, 0, "")

	if err != nil {
		t.Fatal(err)
	}
	warning, normal := page[0], page[1]
	if warning.GetType() != cloudv1.EventType_EVENT_TYPE_WARNING || warning.GetCount() != 3 || warning.GetMessage() != "NodeGroup ng: x" ||
		!warning.GetTime().AsTime().Equal(at) || warning.GetObjectKind() != "Cluster" || warning.GetObjectName() != "prod" {
		t.Errorf("warning = %v", warning)
	}
	if normal.GetType() != cloudv1.EventType_EVENT_TYPE_NORMAL || normal.GetCount() != 1 {
		t.Errorf("normal = %v, want NORMAL counted once", normal)
	}
}
