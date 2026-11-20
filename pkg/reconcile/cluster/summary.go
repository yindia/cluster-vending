package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/clustersummary"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

// The cluster status summary of the node pass (PROP-02 5.2): node totals, the
// groups' allocatable and requested resources, and whether the cluster could
// be read. The pass reads the nodes it already reads and lists the pods once,
// in pages; it keeps no row.
const (
	// summaryPodPages bounds the pod list of one pass: 200 pages of 500.
	summaryPodPages = 200
	// summaryWriteTimeout bounds the write, whatever is left of the pass.
	summaryWriteTimeout = 20 * time.Second
)

var podListKind = schema.GroupVersionKind{Version: "v1", Kind: "PodList"}

// summaryDue claims the cluster's turn: false while it was served less than
// nodeRowsInterval ago (the node rows' throttle interval).
func (s *Reconciler) summaryDue(id string) bool { return s.summary.claim(id, now()) }

// readPods lists the pods once, with limit and continue, adding each page to
// agg as it arrives (a pod is never kept). known: the list was read to the
// end; otherwise the aggregate's requested numbers are partial and must not
// be recorded.
func (s *Reconciler) readPods(ctx context.Context, c client.Client, agg *clustersummary.Aggregator) (known bool, err error) {
	token := ""
	for page := 0; page < summaryPodPages; page++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(podListKind)
		opts := []client.ListOption{client.Limit(nodeListLimit), client.Continue(token)}
		if err := call(ctx, func(ctx context.Context) error { return c.List(ctx, list, opts...) }); err != nil {
			return false, err
		}
		for i := range list.Items {
			agg.AddPodObject(&list.Items[i])
		}
		if token = list.GetContinue(); token == "" {
			return true, nil
		}
	}
	return false, errors.New("too many pods to list")
}

// recordSummary records what the pass read (nodes with agg, pods when known)
// or, when the nodes could not be read (readErr), that the cluster is
// unreachable. It never fails the pass.
func (s *Reconciler) recordSummary(ctx context.Context, row *model.Cluster, agg *clustersummary.Aggregator, podsKnown bool, readErr error) {
	if s.deps.Store == nil {
		return
	}
	at := now()
	var sum cloudv1alpha1.ClusterSummary
	if readErr != nil {
		sum = cloudv1alpha1.ClusterSummary{Reason: unreachableReason(readErr), At: metav1.NewTime(at.UTC())}
	} else {
		sum = agg.Summary(at, podsKnown)
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), summaryWriteTimeout)
	defer cancel()
	if err := s.deps.Store.Clusters().SetSummary(ctx, row.TenantID, row.ID, raw); err != nil {
		s.logger().WarnContext(ctx, "node groups: record the cluster summary", "cluster_id", row.ID, "error", err)
	}
}

// unreachableReason is why a read failed: CREDENTIALS_REJECTED when the
// cluster answered 401 or 403, CLUSTER_UNREACHABLE otherwise (no kubeconfig,
// connect, DNS or TLS errors, a deadline).
func unreachableReason(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		switch status.Status().Code {
		case http.StatusUnauthorized, http.StatusForbidden:
			return cloudv1alpha1.ReasonCredentialsRejected
		}
	}
	return cloudv1alpha1.ReasonClusterUnreachable
}

// noteUnreachable records that the cluster could not be opened.
func (s *Reconciler) noteUnreachable(ctx context.Context, row *model.Cluster, err error) {
	if s.summaryDue(row.ID) {
		s.recordSummary(ctx, row, nil, false, err)
	}
}

// summarize completes the pass's summary: when the nodes were read, the pods
// are listed (once) and the totals recorded; when they could not be, the
// cluster is recorded unreachable. A pass that is not due (nodes.usage nil)
// does nothing.
func (s *Reconciler) summarize(ctx context.Context, c client.Client, row *model.Cluster, nodes nodeCounts) {
	if nodes.usage == nil {
		return
	}
	if nodes.err != nil {
		s.recordSummary(ctx, row, nil, false, nodes.err)
		return
	}
	known, err := s.readPods(ctx, c, nodes.usage)
	if err != nil {
		s.logger().WarnContext(ctx, "node groups: list pods", "cluster_id", row.ID, "error", err)
	}
	s.recordSummary(ctx, row, nodes.usage, known, nil)
}
