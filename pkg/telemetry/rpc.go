// Package telemetry provides bounded-cardinality platform metrics.
package telemetry

import (
	"context"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// RPC measures complete calls, including authentication failures and streams.
// Only registered schema names become labels; arbitrary URLs never do.
type RPC struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight *prometheus.GaugeVec
}

func NewRPC(reg prometheus.Registerer) *RPC {
	labels := []string{"rpc_service", "rpc_method", "rpc_type"}
	r := &RPC{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "cluster_vending_rpc_requests_total", Help: "Completed RPC calls, including streaming calls and authentication failures."}, append(append([]string{}, labels...), "code")),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "cluster_vending_rpc_duration_seconds", Help: "RPC lifetime in seconds; streaming calls include the entire stream lifetime.", Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300}}, labels),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "cluster_vending_rpc_in_flight", Help: "RPC calls currently executing, including open streams."}, labels),
	}
	reg.MustRegister(r.requests, r.duration, r.inflight)
	return r
}

func (r *RPC) Interceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		service, method := "unknown", "unknown"
		if schema, ok := spec.Schema.(protoreflect.MethodDescriptor); ok {
			service, method = string(schema.Parent().FullName()), string(schema.Name())
		}
		typ := spec.StreamType.String()
		switch typ {
		case "unary", "client", "server", "bidi":
		default:
			typ = "unknown"
		}
		labels := []string{service, method, typ}
		start, code := time.Now(), "internal"
		r.inflight.WithLabelValues(labels...).Inc()
		defer func() {
			r.inflight.WithLabelValues(labels...).Dec()
			r.duration.WithLabelValues(labels...).Observe(time.Since(start).Seconds())
			r.requests.WithLabelValues(append(labels, code)...).Inc()
		}()
		err := next(ctx, spec, stream)
		code = "ok"
		if err != nil {
			code = connect.CodeOf(err).String()
		}
		return err
	}
}
