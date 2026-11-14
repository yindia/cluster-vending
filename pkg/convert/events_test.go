package convert

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The kinds list the contract's recent events: the last ten, with times
// to the second (what the API server stores), and none without a time or
// a reason.
func TestRecentEventsOfTheKinds(t *testing.T) {
	at := time.Date(2026, 11, 13, 10, 0, 0, 123456789, time.UTC)
	event := func(reason string, kind cloudv1.EventType) *cloudv1.Event {
		return &cloudv1.Event{Time: timestamppb.New(at), Type: kind, Reason: reason, Message: "about " + reason, Count: 2}
	}
	var many []*cloudv1.Event
	for i := range 12 {
		many = append(many, event(fmt.Sprintf("Reason%d", i), cloudv1.EventType_EVENT_TYPE_NORMAL))
	}
	tests := []struct {
		name string
		in   []*cloudv1.Event
		want []string // "Type Reason"
	}{
		{name: "none"},
		{name: "a warning and a normal one",
			in:   []*cloudv1.Event{event("Failed", cloudv1.EventType_EVENT_TYPE_WARNING), event("Running", cloudv1.EventType_EVENT_TYPE_NORMAL)},
			want: []string{"Warning Failed", "Normal Running"}},
		{name: "an unspecified type is normal", in: []*cloudv1.Event{event("Running", cloudv1.EventType_EVENT_TYPE_UNSPECIFIED)},
			want: []string{"Normal Running"}},
		{name: "no time, no reason: left out",
			in:   []*cloudv1.Event{{Reason: "NoTime"}, {Time: timestamppb.New(at)}, event("Running", cloudv1.EventType_EVENT_TYPE_NORMAL)},
			want: []string{"Normal Running"}},
		{name: "only the last ten", in: many, want: []string{"Normal Reason2", "Normal Reason3", "Normal Reason4", "Normal Reason5",
			"Normal Reason6", "Normal Reason7", "Normal Reason8", "Normal Reason9", "Normal Reason10", "Normal Reason11"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recentEvents(tt.in)

			var have []string
			for _, e := range got {
				have = append(have, e.Type+" "+e.Reason)
				if !e.Time.Time.Equal(at.Truncate(time.Second)) || e.Count != 2 || e.Message != "about "+e.Reason {
					t.Errorf("event = %+v", e)
				}
			}
			if fmt.Sprint(have) != fmt.Sprint(tt.want) {
				t.Fatalf("events = %v, want %v", have, tt.want)
			}
		})
	}
}

// Both kinds' statuses carry them.
func TestStatusIntoCarriesRecentEvents(t *testing.T) {
	at := timestamppb.New(time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC))
	events := []*cloudv1.Event{{Time: at, Type: cloudv1.EventType_EVENT_TYPE_NORMAL, Reason: "Running", Count: 1}}
	var cluster cloudv1alpha1.ClusterStatus
	var network cloudv1alpha1.NetworkStatus

	ClusterStatusInto(&cloudv1.ClusterStatus{RecentEvents: events}, "", metav1.Now(), &cluster)
	NetworkStatusInto(&cloudv1.NetworkStatus{RecentEvents: events}, "", metav1.Now(), &network)

	if len(cluster.RecentEvents) != 1 || cluster.RecentEvents[0].Reason != "Running" ||
		len(network.RecentEvents) != 1 || network.RecentEvents[0].Type != "Normal" {
		t.Fatalf("cluster %+v, network %+v", cluster.RecentEvents, network.RecentEvents)
	}
	// A status without events clears what the kind held.
	ClusterStatusInto(&cloudv1.ClusterStatus{}, "", metav1.Now(), &cluster)
	if cluster.RecentEvents != nil {
		t.Errorf("recent events were kept: %+v", cluster.RecentEvents)
	}
}
