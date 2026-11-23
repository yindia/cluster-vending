// Package live is the live reader of PROP-02 (section 5.5, option D): it
// reads a workload cluster on request, with its own read-only identity, and
// serves sorted, paginated pages from a short-lived materialized view. It is
// the `cluster-vending live` subcommand; the API server calls it and holds no
// workload credentials itself.
package live

import (
	"errors"
	"fmt"
	"math"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The reasons of a failed live read (cloud.v1.LiveReadError.reason).
const (
	ReasonUnreachable           = "CLUSTER_UNREACHABLE"
	ReasonCredentialsRejected   = "CREDENTIALS_REJECTED"
	ReasonNoLiveIdentity        = "NO_LIVE_IDENTITY"
	ReasonBusy                  = "BUSY"
	ReasonLimitExceeded         = "LIMIT_EXCEEDED"
	ReasonAuthRequired          = "AUTHENTICATION_REQUIRED"
	ReasonLiveReaderUnavailable = "LIVE_READER_UNAVAILABLE"
)

// Messages the console shows.
const (
	MessageNoLiveIdentity      = "the platform has no read-only identity for this cluster"
	MessageCredentialsRejected = "the platform cannot read this cluster: its read-only credentials were rejected (the live kubeconfig is refreshed by its ClusterAuth; check the account's reader role and the cluster's live access entry)"
	MessageRestart             = "restart the list"
	MessageAuthRequired        = "live reads need authentication"
)

// Error is a live read that failed, with its reason. A failure that reached
// the cluster (unreachable, rejected, over a cap) starts the cluster's
// cooldown; the others do not.
type Error struct {
	Reason     string
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Reason + ": " + e.Message }

// cools: the failure reached (or tried to reach) the cluster, so repeating it
// at once would only load the cluster again.
func (e *Error) cools() bool {
	switch e.Reason {
	case ReasonUnreachable, ReasonCredentialsRejected, ReasonLimitExceeded:
		return true
	}
	return false
}

func newError(reason, format string, args ...any) *Error {
	return &Error{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// ToConnect is err as the live RPCs answer it: a live Error is UNAVAILABLE
// (FAILED_PRECONDITION without authentication) with a LiveReadError detail;
// a connect error stays; anything else is INTERNAL.
func ToConnect(err error) error {
	if err == nil {
		return nil
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr
	}
	var e *Error
	if !errors.As(err, &e) {
		return connect.NewError(connect.CodeInternal, err.Error())
	}
	code := connect.CodeUnavailable
	if e.Reason == ReasonAuthRequired {
		code = connect.CodeFailedPrecondition
	}
	out := connect.NewError(code, e.Message)
	seconds := int32(min(math.Ceil(e.RetryAfter.Seconds()), math.MaxInt32))
	if detail, derr := connectproto.NewErrorDetail(&cloudv1.LiveReadError{Reason: e.Reason, Message: e.Message, RetryAfterSeconds: seconds}); derr == nil {
		out = out.WithDetail(detail)
	}
	return out
}

// ReasonOf is the LiveReadError reason a connect error carries; "" for none.
func ReasonOf(err error) string {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return ""
	}
	for _, d := range cerr.Details() {
		v, derr := connectproto.UnmarshalErrorDetail(d)
		if derr != nil {
			continue
		}
		if le, ok := v.(*cloudv1.LiveReadError); ok {
			return le.GetReason()
		}
	}
	return ""
}

// errRestart is the answer to a token whose view is gone (expired, evicted,
// from before a restart) or that does not match the request.
func errRestart() error {
	return connect.NewError(connect.CodeInvalidArgument, MessageRestart)
}
