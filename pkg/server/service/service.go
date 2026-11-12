// Package service holds what every cloud.v1 service implementation shares:
// its dependencies (reconcile.Deps) and the mapping of errors to Connect codes. The services
// themselves live in pkg/server/tenant, network and cluster.
package service

import (
	"context"
	"errors"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Deps are what the services work with (reconcile.Deps).
type Deps = reconcile.Deps

// Owner names what objects are created for (reconcile.Owner).
type Owner = reconcile.Owner

// DeleteOrder is the order an owner's objects are deleted in.
type DeleteOrder = reconcile.DeleteOrder

// AllGone reports whether every recorded object is gone from the cluster.
func AllGone(views []*cloudv1.ManagedResource) bool { return reconcile.AllGone(views) }

// SettleMissing turns MISSING into CREATING for objects recorded a moment
// ago (reconcile.SettleMissing).
func SettleMissing(records []model.ManagedResource, views []*cloudv1.ManagedResource) {
	reconcile.SettleMissing(records, views)
}

// Validate checks a request against its protovalidate rules (the
// buf.validate options in idl/), as INVALID_ARGUMENT.
func Validate(req proto.Message) error {
	if err := protovalidate.Validate(req); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return nil
}

// Unimplemented is the answer of every RPC until it is written.
func Unimplemented(method string) error {
	return connect.NewError(connect.CodeUnimplemented, method+" is not implemented yet")
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
// code and message. Other errors pass as they are.
func Faults(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		err := next(ctx, spec, stream)
		if err != nil && faultCode(err) != 0 {
			var codeErr *connect.Error
			if !errors.As(err, &codeErr) {
				return ToConnect(err)
			}
		}
		return err
	}
}
