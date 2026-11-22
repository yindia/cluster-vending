// Package authn applies pkg/auth to the API's requests: a Connect server
// interceptor that reads the bearer token and has it verified. It is
// authentication only: a caller whose token is accepted may call every RPC,
// for every tenant. The handlers read the caller, where they want it, from
// the context (auth.FromContext).
package authn

import (
	"context"
	"log/slog"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/fault"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// Verifier turns a bearer token into the caller it stands for
// (auth.Verifier), or fails with a fault: Unauthenticated for a token
// that is not accepted, Unavailable while its issuer cannot be reached.
type Verifier interface {
	Verify(ctx context.Context, token string) (*auth.Identity, error)
}

// Options are how requests are authenticated.
type Options struct {
	// Mode is auth.ModeNone (also when empty) or auth.ModeRequired.
	Mode auth.Mode
	// Verifier checks the tokens; required unless Mode is none.
	Verifier Verifier
	// Logger receives one line per write and per refused call, with the
	// caller's subject. Never a token. Nil: nothing is logged.
	Logger *slog.Logger
}

const (
	headerAuthorization = "Authorization"
	headerChallenge     = "WWW-Authenticate"
	schemeBearer        = "Bearer"
)

// Interceptor authenticates every RPC:
//
//   - mode none: nothing is checked; the caller is anonymous.
//   - mode required: a request without a token, or with one no issuer of
//     the configuration signed for this API (signature, issuer, audience,
//     expiry), is refused UNAUTHENTICATED before the handler runs.
//
// An accepted caller may call every RPC; nothing is authorized. Writes and
// refusals are logged with the caller's subject and the request's tenant.
// It answers with faults, so it goes inside service.Faults.
func Interceptor(o Options) connect.ServerInterceptor {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			if o.Mode == auth.ModeNone || o.Mode == "" {
				return next(auth.WithIdentity(ctx, auth.Anonymous()), spec, stream)
			}
			return o.serve(ctx, spec, stream, next)
		}
	}
}

// serve is one RPC in mode required.
func (o Options) serve(ctx context.Context, spec connect.Spec, stream connect.ServerStream, next connect.ServerFunc) error {
	id, err := o.authenticate(ctx)
	if err != nil {
		o.Logger.WarnContext(ctx, "api call refused", "procedure", spec.Procedure, "error", err)
		return err
	}
	ctx = auth.WithIdentity(ctx, id)
	method, ok := spec.Schema.(protoreflect.MethodDescriptor)
	if !ok || !auth.Writes(method) {
		// A read, or not an RPC of the API (the server answers
		// UNIMPLEMENTED): nothing to log.
		return next(ctx, spec, stream)
	}
	call := &call{options: o, spec: spec, id: id}
	return call.done(ctx, next(ctx, spec, &recordedStream{ServerStream: stream, call: call}))
}

// authenticate is whom the bearer token of the request in ctx stands for.
func (o Options) authenticate(ctx context.Context) (*auth.Identity, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	header := ""
	if info != nil {
		header = info.RequestHeader().Get(headerAuthorization)
	}
	if header == "" {
		challenge(info)
		return nil, fault.New(fault.Unauthenticated, "this API needs a bearer token: run `cluster-vending login`, or set CV_TOKEN")
	}
	scheme, token, _ := strings.Cut(header, " ")
	token = strings.TrimSpace(token)
	if !strings.EqualFold(scheme, schemeBearer) || token == "" {
		challenge(info)
		return nil, fault.New(fault.Unauthenticated, "the Authorization header must be \"Bearer <token>\"")
	}
	id, err := o.Verifier.Verify(ctx, token)
	if err != nil {
		if fault.Is(err, fault.Unauthenticated) {
			challenge(info)
		}
		return nil, err
	}
	return id, nil
}

// challenge tells the client how to authenticate (RFC 6750).
func challenge(info *connect.CallInfo) {
	if info != nil {
		info.ResponseHeader().Set(headerChallenge, schemeBearer)
	}
}

// call is one authenticated write: who makes it, on which tenant.
type call struct {
	options Options
	spec    connect.Spec
	id      *auth.Identity
	// tenant is the tenant the request named, once it was received.
	tenant string
}

// done logs the end of the write and returns err.
func (c *call) done(ctx context.Context, err error) error {
	outcome := "ok"
	if err != nil {
		outcome = connect.CodeOf(service.ToConnect(err)).String()
	}
	c.options.Logger.InfoContext(ctx, "api write", "procedure", c.spec.Procedure, "subject", c.id.Subject,
		"username", c.id.Username, "issuer", c.id.Issuer, "tenant", c.tenant, "outcome", outcome)
	return err
}

// recordedStream is the stream of a write: it notes the tenant of the
// request for the log. It checks nothing.
type recordedStream struct {
	connect.ServerStream
	call *call
}

// Receive reads the next request and notes its tenant.
func (r *recordedStream) Receive(msg any) error {
	if err := r.ServerStream.Receive(msg); err != nil {
		return err
	}
	if request, ok := msg.(proto.Message); ok {
		r.call.tenant = auth.Tenant(request.ProtoReflect())
	}
	return nil
}
