package live

import (
	"context"
	"errors"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Lister lists one page of a kind of the workload cluster: limit items from
// the continue token cont. The live reader's identity reads with it; nothing
// else of the cluster is reachable through it.
type Lister interface {
	List(ctx context.Context, gvr schema.GroupVersionResource, limit int64, cont string) (*unstructured.UnstructuredList, error)
}

// The kinds a nodes build reads, in order.
var (
	NodesGVR       = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	PodsGVR        = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	NodeClaimsGVR  = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodeclaims"}
	NodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}
)

// errExpired: a continue token expired (410 Gone): the build restarts once.
var errExpired = errors.New("list expired")

// budget counts the pages of one build against the cap.
type budget struct {
	pages, max int
	// read: at least one page came back, so the cluster answered.
	read bool
}

// eachPage lists gvr page by page and calls each for every item; a page is
// never kept beyond its loop. The pages count against b.
func eachPage(ctx context.Context, l Lister, gvr schema.GroupVersionResource, limit int64, b *budget, each func(*unstructured.Unstructured)) error {
	token := ""
	for {
		if b.pages >= b.max {
			return newError(ReasonLimitExceeded, "the read stopped after %d pages", b.max)
		}
		b.pages++
		list, err := l.List(ctx, gvr, limit, token)
		if err != nil {
			if token == "" && (apierrors.IsNotFound(err) || errors.Is(err, errNotServed)) {
				return errNotServed
			}
			return classify(ctx, err, b.read, token != "")
		}
		b.read = true
		for i := range list.Items {
			each(&list.Items[i])
		}
		if token = list.GetContinue(); token == "" {
			return nil
		}
	}
}

// classify is a list error as a live Error. read: the cluster already
// answered a page of this build; continued: the failed call carried a
// continue token.
func classify(ctx context.Context, err error, read, continued bool) error {
	var status apierrors.APIStatus
	switch {
	case continued && (apierrors.IsResourceExpired(err) || apierrors.IsGone(err)):
		return errExpired
	case errors.As(err, &status) && (status.Status().Code == http.StatusUnauthorized || status.Status().Code == http.StatusForbidden):
		return &Error{Reason: ReasonCredentialsRejected, Message: MessageCredentialsRejected}
	case ctx.Err() != nil && read:
		return newError(ReasonLimitExceeded, "the read did not finish within its deadline")
	case ctx.Err() != nil:
		return newError(ReasonUnreachable, "the workload cluster did not answer within the deadline")
	}
	return newError(ReasonUnreachable, "the workload cluster cannot be reached: %v", err)
}

// errNotServed: the cluster does not serve the kind (no CRD, no API).
var errNotServed = errors.New("not served")
