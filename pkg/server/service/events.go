package service

import (
	"strconv"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Page sizes of the event lists.
const (
	defaultEventsPage = 50
	maxEventsPage     = 200
)

// warningType is how Kubernetes spells the type of a warning event.
const warningType = "Warning"

// EventsPage is one page of an object's events as the API returns them:
// events is the whole list, newest first (the store's order); only the
// warnings when onlyWarnings. token is the next_page_token of the page
// before ("" for the first); next is the one of the page after, "" when
// this is the last.
func EventsPage(events []model.Event, onlyWarnings bool, pageSize int32, token string) (page []*cloudv1.Event, next string, err error) {
	if onlyWarnings {
		var warnings []model.Event
		for _, e := range events {
			if e.Type == warningType {
				warnings = append(warnings, e)
			}
		}
		events = warnings
	}
	start := 0
	if token != "" {
		if start, err = strconv.Atoi(token); err != nil || start < 0 || start > len(events) {
			return nil, "", connect.NewError(connect.CodeInvalidArgument, "page_token is not a token of this list")
		}
	}
	size := int(pageSize)
	if size <= 0 {
		size = defaultEventsPage
	}
	end := min(start+min(size, maxEventsPage), len(events))
	page = make([]*cloudv1.Event, 0, end-start)
	for _, e := range events[start:end] {
		kind := cloudv1.EventType_EVENT_TYPE_NORMAL
		if e.Type == warningType {
			kind = cloudv1.EventType_EVENT_TYPE_WARNING
		}
		page = append(page, &cloudv1.Event{Time: timestamppb.New(e.Time), Type: kind, Reason: e.Reason, Message: e.Message,
			Count: max(e.Count, 1), ObjectKind: e.Kind, ObjectName: e.Name})
	}
	if end < len(events) {
		next = strconv.Itoa(end)
	}
	return page, next, nil
}
