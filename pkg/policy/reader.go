package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/yindia/cluster-vending/pkg/deprecation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Reader uses short-lived bounded lists; no workload watch, informer, or write.
type Reader struct {
	// Full collects for the results bucket: lists page 500 at a time with no
	// MaxRows cap on what is kept, until the pass deadline (FullTimeout),
	// where the snapshot is marked truncated. Off, the snapshot is bounded
	// for the Cluster object.
	Full        bool
	Dynamic     dynamic.Interface
	Constraints func(context.Context) (*metav1.APIResourceList, error)
}

func NewReader(kubeconfig []byte) (*Reader, error) {
	cfg, err := deprecation.RESTConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 10 * time.Second
	d, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Reader{Dynamic: d, Constraints: func(ctx context.Context) (*metav1.APIResourceList, error) {
		out := &metav1.APIResourceList{}
		err := disco.RESTClient().Get().AbsPath("/apis/constraints.gatekeeper.sh/v1beta1").Do(ctx).Into(out)
		return out, err
	}}, nil
}
func readNotice(p *cloudv1.ClusterPolicySnapshot, source string, err error) {
	// Do not persist raw transport errors (URLs or credentials) in status.
	why := "read failed"
	switch {
	case apierrors.IsNotFound(err):
		why = "API unavailable or not installed"
	case apierrors.IsForbidden(err):
		why = "access forbidden"
	case apierrors.IsUnauthorized(err):
		why = "authentication failed"
	case errors.Is(err, context.DeadlineExceeded):
		why = "read timed out"
	}
	notice(p, source+": "+why)
}
func (r *Reader) Collect(ctx context.Context, now time.Time) *cloudv1.ClusterPolicySnapshot {
	timeout, pageSize, maxPages, maxRows, requestBudget, eventReserve := Timeout, int64(100), 4, MaxRows, 48, 8
	if r.Full {
		timeout, pageSize, maxPages, maxRows, requestBudget, eventReserve = FullTimeout, FullPageSize, fullPageLimit, fullMaxRows, fullRequests, fullEventShare
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p := &cloudv1.ClusterPolicySnapshot{Summary: &cloudv1.ClusterPolicySnapshotSummary{CollectedAt: timestamp(now), Notices: []string{"Reports are engine observations, not proof of admission denial. Missing reports do not establish compliance.", "Native VAP violations require an external PolicyReport producer; native audit logs are not collected."}}}
	requests := 0
	successfulLists := 0
	// Events are reserved a share of the shared budget so that reports and
	// Gatekeeper constraints can never starve them.
	budget := requestBudget - eventReserve
	list := func(gvr schema.GroupVersionResource, selector string, parse func(*unstructured.Unstructured)) {
		token := ""
		for page := 0; page < maxPages; page++ {
			if requests >= budget || ctx.Err() != nil {
				notice(p, "Collection request or time budget exhausted before reading "+gvr.String()+".")
				p.Summary.Truncated = true
				return
			}
			requests++
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			result, err := r.Dynamic.Resource(gvr).List(call, metav1.ListOptions{Limit: pageSize, Continue: token, FieldSelector: selector})
			cancel()
			if err != nil {
				readNotice(p, gvr.String(), err)
				return
			}
			successfulLists++
			for i := range result.Items {
				if i >= int(pageSize) {
					p.Summary.Truncated = true
					break
				}
				parse(&result.Items[i])
			}
			token = result.GetContinue()
			if token == "" {
				return
			}
		}
		p.Summary.Truncated = true
		notice(p, "Kubernetes list page limit reached for "+gvr.String()+".")
	}
	for _, gv := range []schema.GroupVersion{{Group: "wgpolicyk8s.io", Version: "v1alpha2"}, {Group: "openreports.io", Version: "v1alpha1"}} {
		for _, resource := range []string{"policyreports", "clusterpolicyreports"} {
			// OpenReports uses Report/ClusterReport rather than PolicyReport.
			if gv.Group == "openreports.io" {
				resource = strings.ReplaceAll(resource, "policy", "")
			}
			list(gv.WithResource(resource), "", func(u *unstructured.Unstructured) { reportLimit(p, u, maxRows) })
		}
	}
	if r.Constraints != nil {
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		resources, err := r.Constraints(call)
		cancel()
		if err != nil {
			readNotice(p, "Gatekeeper constraint discovery", err)
		} else if resources != nil {
			var names []string
			for _, res := range resources.APIResources {
				// Subresources such as kind/status are not constraint kinds.
				if !strings.Contains(res.Name, "/") {
					names = append(names, res.Name)
				}
			}
			sort.Strings(names)
			if len(names) > 32 {
				p.Summary.Truncated = true
				names = names[:32]
			}
			for _, name := range names {
				list(schema.GroupVersionResource{Group: "constraints.gatekeeper.sh", Version: "v1beta1", Resource: name}, "", func(u *unstructured.Unstructured) { constraintLimit(p, u, maxRows) })
			}
		}
	}
	budget = requestBudget
	seen := map[string]bool{}
	var events []*cloudv1.ClusterViolationEvent
	for _, reason := range []string{"PolicyViolation", "FailedAdmission", "WarningAdmission", "DryrunViolation"} {
		list(schema.GroupVersionResource{Version: "v1", Resource: "events"}, "reason="+reason, func(u *unstructured.Unstructured) {
			e := event(u)
			if e == nil || seen[e.Id] {
				return
			}
			seen[e.Id] = true
			if e.LastObservedAt == nil {
				notice(p, "An admission event has no usable timestamp.")
				return
			}
			if e.LastObservedAt.AsTime().Before(now.Add(-EventTTL)) || e.LastObservedAt.AsTime().After(now.Add(time.Minute)) {
				return
			}
			events = append(events, e) // bounded by the request budget and page size
		})
	}
	sort.SliceStable(p.Violations, func(i, j int) bool { return p.Violations[i].Id < p.Violations[j].Id })
	// Newest first, then cap: the API list order says nothing about recency.
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if a.LastObservedAt.AsTime().Equal(b.LastObservedAt.AsTime()) {
			return a.Id < b.Id
		}
		return a.LastObservedAt.AsTime().After(b.LastObservedAt.AsTime())
	})
	if len(events) > maxRows {
		p.Summary.Truncated = true
		events = events[:maxRows]
	}
	p.Events = events
	if p.Summary.Truncated {
		notice(p, fmt.Sprintf("Collection is bounded to %d findings and %d events; lists may be partial.", maxRows, maxRows))
	}
	if successfulLists == 0 {
		p.Summary.CollectedAt = nil
		notice(p, "No workload policy data could be read.")
	}
	return p
}
