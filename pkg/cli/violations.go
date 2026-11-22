package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// maxViolationPages bounds how many pages one command reads.
const maxViolationPages = 20

// ClusterViolations is cluster violations: the policy violations the
// cluster's engines report (ListClusterViolations), or with --events the
// policy and admission events of the last hour (ListClusterViolationEvents).
// --namespace and --engine filter, --sort and --order order; every page is
// read unless --page-size asks for one.
func ClusterViolations(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	if o.Events {
		return violationEvents(ctx, c, o)
	}
	by, order, err := sortOf[cloudv1.ViolationSortBy](o, "VIOLATION_SORT_BY_")
	if err != nil {
		return nil, err
	}
	req := &cloudv1.ListClusterViolationsRequest{TenantId: o.Tenant, ClusterId: o.ID, Namespace: o.Namespace,
		Engine: o.Engine, PageSize: o.PageSize, PageToken: o.PageToken, SortBy: by, SortOrder: order}
	var all *cloudv1.ListClusterViolationsResponse
	for range maxViolationPages {
		res, err := call(ctx, req, c.Cluster.ListClusterViolations)
		if err != nil {
			return nil, err
		}
		page := res.(*cloudv1.ListClusterViolationsResponse)
		if all == nil {
			all = proto.CloneOf(page)
		} else {
			all.Violations = append(all.Violations, page.GetViolations()...)
			all.NextPageToken = page.GetNextPageToken()
		}
		if page.GetNextPageToken() == "" || o.PageSize > 0 {
			break
		}
		req.PageToken = page.GetNextPageToken()
	}
	if o.PageSize == 0 {
		all.NextPageToken = ""
	}
	if !isTable(o.Output) {
		return all, nil
	}
	return nil, printViolations(o.Out, all, o.Output == OutputWide)
}

func violationEvents(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	by, order, err := sortOf[cloudv1.ViolationEventSortBy](o, "VIOLATION_EVENT_SORT_BY_")
	if err != nil {
		return nil, err
	}
	req := &cloudv1.ListClusterViolationEventsRequest{TenantId: o.Tenant, ClusterId: o.ID, Namespace: o.Namespace,
		Engine: o.Engine, PageSize: o.PageSize, PageToken: o.PageToken, SortBy: by, SortOrder: order}
	var all *cloudv1.ListClusterViolationEventsResponse
	for range maxViolationPages {
		res, err := call(ctx, req, c.Cluster.ListClusterViolationEvents)
		if err != nil {
			return nil, err
		}
		page := res.(*cloudv1.ListClusterViolationEventsResponse)
		if all == nil {
			all = proto.CloneOf(page)
		} else {
			all.Events = append(all.Events, page.GetEvents()...)
			all.NextPageToken = page.GetNextPageToken()
		}
		if page.GetNextPageToken() == "" || o.PageSize > 0 {
			break
		}
		req.PageToken = page.GetNextPageToken()
	}
	if o.PageSize == 0 {
		all.NextPageToken = ""
	}
	if !isTable(o.Output) {
		return all, nil
	}
	return nil, printViolationEvents(o.Out, all, o.Output == OutputWide)
}

// policyHead writes when the rows are from (live or collected), the
// collection's state and its notices.
func policyHead(w io.Writer, count string, live *timestamppb.Timestamp, sum *cloudv1.ClusterPolicySnapshotSummary, notices []string, next string) error {
	head := count
	switch {
	case live != nil:
		head += ", live as of " + live.AsTime().UTC().Format("2006-01-02 15:04:05") + " UTC"
	case sum.GetCollectedAt() != nil:
		head += ", collected " + sum.GetCollectedAt().AsTime().UTC().Format("2006-01-02 15:04:05") + " UTC"
	default:
		head += "; the controller has not collected the cluster's policy reports yet"
	}
	var flags []string
	if sum.GetStale() {
		flags = append(flags, "stale")
	}
	if sum.GetIncomplete() {
		flags = append(flags, "incomplete")
	}
	if sum.GetTruncated() {
		flags = append(flags, "truncated")
	}
	if len(flags) > 0 {
		head += " (" + strings.Join(flags, ", ") + ")"
	}
	if _, err := fmt.Fprintln(w, head); err != nil {
		return err
	}
	for _, n := range append(append([]string(nil), sum.GetNotices()...), notices...) {
		if _, err := fmt.Fprintln(w, "Notice: "+n); err != nil {
			return err
		}
	}
	if next != "" {
		if _, err := fmt.Fprintln(w, "Next page: --page-token "+next); err != nil {
			return err
		}
	}
	return nil
}

func printViolations(w io.Writer, res *cloudv1.ListClusterViolationsResponse, wide bool) error {
	count := strconv.Itoa(len(res.GetViolations())) + " violations"
	if err := policyHead(w, count, res.GetLiveAt(), res.GetSummary(), res.GetNotices(), res.GetNextPageToken()); err != nil {
		return err
	}
	if len(res.GetViolations()) == 0 {
		return nil
	}
	header := []string{"NAMESPACE", "RESOURCE", "POLICY", "RULE", "ENGINE", "SEVERITY", "RESULT", "MESSAGE"}
	if wide {
		header = append(header, "ENFORCEMENT", "OBSERVED")
	}
	rows := make([][]string, 0, len(res.GetViolations()))
	for _, v := range res.GetViolations() {
		r := []string{orNone(v.GetResource().GetNamespace()), objectRef(v.GetResource()), objectRef(v.GetPolicy()),
			orNone(v.GetRule()), orNone(v.GetEngine()), orNone(v.GetSeverity()), orNone(v.GetResult()), orNone(oneLine(v.GetMessage()))}
		if wide {
			r = append(r, orNone(v.GetEnforcement()), age(v.GetObservedAt()))
		}
		rows = append(rows, r)
	}
	return table(w, header, rows)
}

func printViolationEvents(w io.Writer, res *cloudv1.ListClusterViolationEventsResponse, wide bool) error {
	count := strconv.Itoa(len(res.GetEvents())) + " policy events in the last hour"
	if err := policyHead(w, count, res.GetLiveAt(), res.GetSummary(), res.GetNotices(), res.GetNextPageToken()); err != nil {
		return err
	}
	if n := res.GetRetentionNotice(); n != "" {
		if _, err := fmt.Fprintln(w, "Notice: "+n); err != nil {
			return err
		}
	}
	if len(res.GetEvents()) == 0 {
		return nil
	}
	header := []string{"LAST SEEN", "COUNT", "REASON", "NAMESPACE", "RESOURCE", "POLICY", "ACTION", "MESSAGE"}
	if wide {
		header = append(header, "ENGINE", "ENFORCEMENT")
	}
	rows := make([][]string, 0, len(res.GetEvents()))
	for _, e := range res.GetEvents() {
		r := []string{age(e.GetLastObservedAt()), strconv.Itoa(int(e.GetCount())), orNone(e.GetReason()),
			orNone(e.GetResource().GetNamespace()), objectRef(e.GetResource()), objectRef(e.GetPolicy()),
			orNone(e.GetAction()), orNone(oneLine(e.GetMessage()))}
		if wide {
			r = append(r, orNone(e.GetEngine()), orNone(e.GetEnforcement()))
		}
		rows = append(rows, r)
	}
	return table(w, header, rows)
}

// objectRef is Kind/name, or <none>.
func objectRef(r *cloudv1.PolicyObjectReference) string {
	switch {
	case r.GetName() == "":
		return none
	case r.GetKind() == "":
		return r.GetName()
	}
	return r.GetKind() + "/" + r.GetName()
}
