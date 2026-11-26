package telemetry

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

// The reasons an API call is refused before its handler runs.
const (
	AuthMissingToken    = "missing_token"
	AuthMalformedHeader = "malformed_header"
	AuthRejected        = "rejected"
	AuthUnavailable     = "issuer_unavailable"
)

var authReasons = []string{AuthMissingToken, AuthMalformedHeader, AuthRejected, AuthUnavailable}

// Auth counts refused authentications by reason.
type Auth struct{ failures *prometheus.CounterVec }

// NewAuth registers cluster_vending_auth_failures_total on reg.
func NewAuth(reg prometheus.Registerer) *Auth {
	a := &Auth{failures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "cluster_vending_auth_failures_total",
		Help: "API calls refused by authentication, by reason: missing_token, malformed_header, rejected (signature, issuer, audience or expiry), issuer_unavailable."},
		[]string{"reason"})}
	reg.MustRegister(a.failures)
	for _, r := range authReasons {
		a.failures.WithLabelValues(r)
	}
	return a
}

// Failed counts one refusal; an unknown reason is "other".
func (a *Auth) Failed(reason string) {
	for _, r := range authReasons {
		if r == reason {
			a.failures.WithLabelValues(r).Inc()
			return
		}
	}
	a.failures.WithLabelValues(labelOther).Inc()
}

// Store measures the store's calls to the Kubernetes API: the platform's
// custom resources it keeps its rows in (pkg/database/cloudstore).
type Store struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewStore registers cluster_vending_store_* on reg.
func NewStore(reg prometheus.Registerer) *Store {
	s := &Store{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "cluster_vending_store_requests_total",
			Help: "Store calls to the Kubernetes API by operation, custom resource kind and outcome (ok, not_found, conflict, already_exists, invalid, forbidden, timeout, error)."},
			[]string{"operation", "kind", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "cluster_vending_store_request_duration_seconds",
			Help: "Duration of the store's calls to the Kubernetes API.", Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}},
			[]string{"operation", "kind"}),
	}
	reg.MustRegister(s.requests, s.duration)
	return s
}

// storeKinds are the kinds the store keeps (cloudstore.Kinds); anything
// else is "other".
var storeKinds = map[string]bool{"Tenant": true, "SubTenant": true, "CloudAccount": true, "Network": true, "Cluster": true,
	"Release": true, "ReleaseChannel": true, "ReleaseHold": true, "Component": true, "ComponentDefinition": true}

func (s *Store) kind(scheme *runtime.Scheme, obj runtime.Object) string {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil || gvk.Group != cloud.GroupVersion.Group {
		return labelOther
	}
	kind := gvk.Kind
	if len(kind) > 4 && kind[len(kind)-4:] == "List" {
		kind = kind[:len(kind)-4]
	}
	if !storeKinds[kind] {
		return labelOther
	}
	return kind
}

// Outcome is the bounded class of a Kubernetes API error.
func Outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case apierrors.IsNotFound(err):
		return "not_found"
	case apierrors.IsConflict(err):
		return "conflict"
	case apierrors.IsAlreadyExists(err):
		return "already_exists"
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return "invalid"
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return "forbidden"
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "error"
	}
}

func (s *Store) observe(operation, kind string, start time.Time, err error) error {
	s.duration.WithLabelValues(operation, kind).Observe(time.Since(start).Seconds())
	s.requests.WithLabelValues(operation, kind, Outcome(err)).Inc()
	return err
}

// noWatch is a client that cannot watch: the store never does.
type noWatch struct{ client.Client }

func (noWatch) Watch(context.Context, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
	return nil, errors.New("the store's client does not watch")
}

// Wrap returns c measuring every call it makes.
func (s *Store) Wrap(c client.Client) client.Client {
	scheme := c.Scheme()
	return interceptor.NewClient(noWatch{c}, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			start := time.Now()
			return s.observe("get", s.kind(scheme, obj), start, c.Get(ctx, key, obj, opts...))
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			start := time.Now()
			return s.observe("list", s.kind(scheme, list), start, c.List(ctx, list, opts...))
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			start := time.Now()
			return s.observe("create", s.kind(scheme, obj), start, c.Create(ctx, obj, opts...))
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			start := time.Now()
			return s.observe("update", s.kind(scheme, obj), start, c.Update(ctx, obj, opts...))
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			start := time.Now()
			return s.observe("patch", s.kind(scheme, obj), start, c.Patch(ctx, obj, patch, opts...))
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			start := time.Now()
			return s.observe("delete", s.kind(scheme, obj), start, c.Delete(ctx, obj, opts...))
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			start := time.Now()
			return s.observe("update_"+subresource(sub), s.kind(scheme, obj), start, c.SubResource(sub).Update(ctx, obj, opts...))
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			start := time.Now()
			return s.observe("patch_"+subresource(sub), s.kind(scheme, obj), start, c.SubResource(sub).Patch(ctx, obj, patch, opts...))
		},
	})
}

func subresource(name string) string {
	if name == "status" {
		return "status"
	}
	return labelOther
}

// LiveClient measures the API server's calls to the live reader.
type LiveClient struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight prometheus.Gauge
}

// NewLiveClient registers cluster_vending_live_client_* on reg.
func NewLiveClient(reg prometheus.Registerer) *LiveClient {
	l := &LiveClient{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "cluster_vending_live_client_requests_total",
			Help: "The API server's calls to the live reader by method and outcome: ok, timeout, unreachable, or the reader's own error code."},
			[]string{"method", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "cluster_vending_live_client_request_duration_seconds",
			Help: "Duration of the API server's calls to the live reader.", Buckets: []float64{.01, .05, .1, .25, .5, 1, 2, 3, 5, 10, 15, 30}},
			[]string{"method"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{Name: "cluster_vending_live_client_in_flight",
			Help: "The API server's calls to the live reader executing now."}),
	}
	reg.MustRegister(l.requests, l.duration, l.inflight)
	return l
}

// Start begins one call of method; the returned func ends it with its error.
func (l *LiveClient) Start(method string) func(error) {
	start := time.Now()
	l.inflight.Inc()
	return func(err error) {
		l.inflight.Dec()
		l.duration.WithLabelValues(method).Observe(time.Since(start).Seconds())
		l.requests.WithLabelValues(method, liveOutcome(err)).Inc()
	}
}

// liveOutcome is how a live call ended: the reader's own verdict is its
// code; a call that did not reach an answer is timeout or unreachable.
func liveOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) && cerr.IsRemote() {
		if cerr.Code() == connect.CodeDeadlineExceeded {
			return "timeout"
		}
		return cerr.Code().String()
	}
	return "unreachable"
}
