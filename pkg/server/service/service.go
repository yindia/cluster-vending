// Package service holds what every cloud.v1 service implementation shares:
// request validation and the mapping of errors to Connect codes. The
// services themselves live in pkg/server/tenant, network and cluster; their
// dependencies are reconcile.Deps.
package service

import (
	"context"
	"errors"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
)

// Validate checks a request against its protovalidate rules (the
// buf.validate options in idl/), as INVALID_ARGUMENT.
func Validate(req proto.Message) error {
	if err := protovalidate.Validate(req); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return nil
}

// ToConnect maps repository errors to Connect codes. Unknown errors become
// INTERNAL without their text, which may hold database details.
func ToConnect(err error) error {
	var codeErr *connect.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &codeErr):
		return err
	case faultCode(err) != 0:
		f, _ := fault.As(err)
		return connect.NewError(faultCode(err), f.Message)
	case errors.Is(err, repository.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err.Error())
	case errors.Is(err, repository.ErrAlreadyExists):
		return connect.NewError(connect.CodeAlreadyExists, err.Error())
	case errors.Is(err, repository.ErrConflict):
		return connect.NewError(connect.CodeAborted, err.Error())
	case errors.Is(err, repository.ErrInUse):
		return connect.NewError(connect.CodeFailedPrecondition, err.Error())
	case errors.Is(err, repository.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err.Error())
	default:
		return connect.NewError(connect.CodeInternal, "internal error")
	}
}

// faultCodes are the Connect codes of the fault kinds.
var faultCodes = map[fault.Kind]connect.Code{
	fault.Internal: connect.CodeInternal, fault.Invalid: connect.CodeInvalidArgument,
	fault.FailedPrecondition: connect.CodeFailedPrecondition, fault.Unavailable: connect.CodeUnavailable,
	fault.NotFound: connect.CodeNotFound, fault.AlreadyExists: connect.CodeAlreadyExists,
	fault.Aborted: connect.CodeAborted, fault.Unimplemented: connect.CodeUnimplemented,
	fault.PermissionDenied: connect.CodePermissionDenied, fault.Unauthenticated: connect.CodeUnauthenticated,
}

// faultCode is the Connect code of the fault in err's chain; 0 without one.
func faultCode(err error) connect.Code {
	f, ok := fault.As(err)
	if !ok {
		return 0
	}
	return faultCodes[f.Kind]
}

// Faults is the server interceptor that answers an RPC failing with a
// fault (from code that knows no RPC library) with the fault's Connect
// code and message, and one that ended because its context did (the
// client went away, or a deadline passed) with CANCELED or
// DEADLINE_EXCEEDED, however deep the context error was wrapped. Connect
// errors and other errors pass as they are.
func Faults(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		return answer(next(ctx, spec, stream))
	}
}

// answer is the error an RPC returns for err (see Faults).
func answer(err error) error {
	if err == nil {
		return nil
	}
	var codeErr *connect.Error
	if errors.As(err, &codeErr) {
		return err
	}
	if faultCode(err) != 0 || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ToConnect(err)
	}
	return err
}
