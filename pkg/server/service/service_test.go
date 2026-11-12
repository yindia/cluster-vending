package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
)

func TestToConnect(t *testing.T) {
	tests := []struct {
		err  error
		want connect.Code
	}{
		{fmt.Errorf("network: %w", repository.ErrNotFound), connect.CodeNotFound},
		{repository.ErrAlreadyExists, connect.CodeAlreadyExists},
		{repository.ErrConflict, connect.CodeAborted},
		{repository.ErrInUse, connect.CodeFailedPrecondition},
		{repository.ErrInvalid, connect.CodeInvalidArgument},
		{context.Canceled, connect.CodeCanceled},
		{context.DeadlineExceeded, connect.CodeDeadlineExceeded},
		{connect.NewError(connect.CodeUnavailable, "x"), connect.CodeUnavailable},
		{errors.New("pq: password=secret"), connect.CodeInternal},
	}
	for _, tt := range tests {
		if got := connect.CodeOf(ToConnect(tt.err)); got != tt.want {
			t.Errorf("ToConnect(%v) code = %v, want %v", tt.err, got, tt.want)
		}
	}
	if ToConnect(nil) != nil {
		t.Error("ToConnect(nil) != nil")
	}
	if msg := ToConnect(errors.New("pq: password=secret")).Error(); msg != "internal: internal error" {
		t.Errorf("internal error leaks details: %q", msg)
	}
}

// A fault from code that knows no RPC library is answered with its code
// and message.
func TestToConnectMapsFaults(t *testing.T) {
	for kind, want := range map[fault.Kind]connect.Code{
		fault.Internal: connect.CodeInternal, fault.Invalid: connect.CodeInvalidArgument,
		fault.FailedPrecondition: connect.CodeFailedPrecondition, fault.Unavailable: connect.CodeUnavailable,
		fault.NotFound: connect.CodeNotFound, fault.AlreadyExists: connect.CodeAlreadyExists,
		fault.Aborted: connect.CodeAborted, fault.Unimplemented: connect.CodeUnimplemented,
	} {
		err := ToConnect(fmt.Errorf("apply: %w", fault.New(kind, "what the caller reads")))
		var codeErr *connect.Error
		if !errors.As(err, &codeErr) || codeErr.Code() != want || codeErr.Message() != "what the caller reads" {
			t.Errorf("kind %d: %v, want %s with the fault's message", kind, err, want)
		}
	}
}

// An RPC that ended because its context did answers CANCELED or
// DEADLINE_EXCEEDED, also when the context error is wrapped (a store call
// inside a reconciler returns it as it is since the split); a fault keeps
// its code, a Connect error passes as it is.
func TestFaultsAnswersEndedContexts(t *testing.T) {
	ok := connect.NewError(connect.CodeNotFound, "missing")
	for name, tt := range map[string]struct {
		err  error
		want connect.Code
	}{
		"canceled":          {context.Canceled, connect.CodeCanceled},
		"wrapped canceled":  {fmt.Errorf("list networks: %w", context.Canceled), connect.CodeCanceled},
		"deadline":          {fmt.Errorf("observe: %w", context.DeadlineExceeded), connect.CodeDeadlineExceeded},
		"fault":             {fault.New(fault.Unavailable, "down"), connect.CodeUnavailable},
		"connect error":     {ok, connect.CodeNotFound},
		"fault in a cancel": {fmt.Errorf("%w: %w", fault.New(fault.Invalid, "bad"), context.Canceled), connect.CodeInvalidArgument},
	} {
		h := Faults(func(context.Context, connect.Spec, connect.ServerStream) error { return tt.err })
		if got := connect.CodeOf(h(t.Context(), connect.Spec{}, nil)); got != tt.want {
			t.Errorf("%s: %s, want %s", name, got, tt.want)
		}
	}
	if Faults(func(context.Context, connect.Spec, connect.ServerStream) error { return nil })(t.Context(), connect.Spec{}, nil) != nil {
		t.Error("a successful RPC got an error")
	}
}
