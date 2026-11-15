package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// slowCommand is an API call that takes until its context ends.
func slowCommand(ctx context.Context, _ Clients, _ Options) (proto.Message, error) {
	return call(ctx, &cloudv1.WhoAmIRequest{}, func(ctx context.Context, _ *cloudv1.WhoAmIRequest) (*cloudv1.WhoAmIResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
}

func TestRequestTimeoutBoundsOneCall(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		within  time.Duration
	}{
		{"the option", 20 * time.Millisecond, 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()

			err := Run(t.Context(), slowCommand, Clients{}, Options{RequestTimeout: tt.timeout}, nil, OutputYAML)

			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > tt.within {
				t.Fatalf("Run = %v after %s, want a deadline within %s", err, time.Since(start), tt.within)
			}
		})
	}
}

// Without the option a call gets the default, not the old 30 seconds.
func TestRequestTimeoutDefault(t *testing.T) {
	if got := requestTimeoutOf(t.Context()); got != DefaultRequestTimeout || got < 2*time.Minute {
		t.Fatalf("default request timeout = %s", got)
	}
}

// Only the per-call context bounds a call (--request-timeout), not a total
// client timeout.
func TestHTTPClientHasNoTotalTimeout(t *testing.T) {
	if c := newHTTPClient(); c.Timeout != 0 {
		t.Fatalf("http client timeout = %s, want none", c.Timeout)
	}
}
