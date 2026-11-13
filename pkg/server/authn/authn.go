// Package authn applies pkg/auth to the API's requests: a Connect server
// interceptor that reads the bearer token, has it verified, and refuses
// the calls the caller's roles do not allow. The handlers stay as they
// are: they read the caller, where they want it, from the context
// (auth.FromContext).
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
	// Mode is auth.ModeNone (also when empty), ModeOptional or
	// ModeRequired.
	Mode auth.Mode
	// Verifier checks the tokens; required unless Mode is none.
	Verifier Verifier
	// Logger receives one line per write and per refused call. Never a
	// token. Nil: nothing is logged.
	Logger *slog.Logger
}

const (
	headerAuthorization = "Authorization"
	headerChallenge     = "WWW-Authenticate"
	schemeBearer        = "Bearer"
)

// Interceptor authenticates and authorizes every RPC:
//
//   - mode none: nothing is checked; the caller is anonymous.
//   - a request without a token: anonymous in mode optional, refused
//     (UNAUTHENTICATED) in mode required.
//   - a request with a token: the token must verify, in both modes.
//   - the caller must be allowed what the RPC needs (auth.Classify): a
//     platform RPC is checked before the handler runs, a tenant RPC when
//     the handler receives the request, against the tenant_id in it. The
//     handler never sees a request the caller may not make.
//
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

// serve is one RPC in mode optional or required.
func (o Options) serve(ctx context.Context, spec connect.Spec, stream connect.ServerStream, next connect.ServerFunc) error {
	id, err := o.authenticate(ctx)
	if err != nil {
		o.Logger.WarnContext(ctx, "api call refused", "procedure", spec.Procedure, "error", err)
		return err
	}
	ctx = auth.WithIdentity(ctx, id)
	method, ok := spec.Schema.(protoreflect.MethodDescriptor)
	if !ok {
		// Not an RPC of the API (an unknown procedure): the server answers
		// UNIMPLEMENTED, there is nothing to protect.
		return next(ctx, spec, stream)
	}
	call := &call{options: o, spec: spec, id: id}
	access, err := auth.Classify(method)
	if err != nil {
		o.Logger.ErrorContext(ctx, "unclassified RPC refused", "procedure", spec.Procedure, "error", err)
		return call.done(ctx, fault.New(fault.PermissionDenied, "this call is not open to callers"))
	}
	call.access = access
	if access.Scope == auth.ScopeTenant {
		stream = &guardedStream{ServerStream: stream, call: call}
	} else if err := id.Authorize(access, ""); err != nil {
		return call.done(ctx, err)
	}
	return call.done(ctx, next(ctx, spec, stream))
}

// authenticate is the caller of the request in ctx: anonymous without a
// token where the mode allows it, otherwise whom the token stands for.
func (o Options) authenticate(ctx context.Context) (*auth.Identity, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	header := ""
	if info != nil {
		header = info.RequestHeader().Get(headerAuthorization)
	}
	if header == "" {
		if o.Mode == auth.ModeRequired {
			challenge(info)
			return nil, fault.New(fault.Unauthenticated, "this API needs a bearer token: run `cluster-vending login`, or set CV_TOKEN")
		}
		return auth.Anonymous(), nil
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

// call is one authenticated RPC: what it needs and who makes it.
type call struct {
	options Options
	spec    connect.Spec
	id      *auth.Identity
	access  auth.Access
	// tenant is the tenant the request named, once it was received.
	tenant string
}

// done logs the end of the call when it is a write or was refused, and
// returns err.
func (c *call) done(ctx context.Context, err error) error {
	denied := fault.Is(err, fault.PermissionDenied)
	if !c.access.Write && !denied {
		return err
	}
	subject, issuer := c.id.Subject, c.id.Issuer
	if c.id.Anonymous {
		subject = "anonymous"
	}
	outcome := "ok"
	if err != nil {
		outcome = connect.CodeOf(service.ToConnect(err)).String()
	}
	attrs := []any{"procedure", c.spec.Procedure, "subject", subject, "username", c.id.Username, "issuer", issuer,
		"tenant", c.tenant, "outcome", outcome}
	if denied {
		c.options.Logger.WarnContext(ctx, "api call denied", attrs...)
		return err
	}
	c.options.Logger.InfoContext(ctx, "api write", attrs...)
	return err
}

// guardedStream is the stream of a tenant RPC: every request the handler
// receives is checked against the caller's tenants first.
type guardedStream struct {
	connect.ServerStream
	call *call
}

// Receive reads the next request and refuses it (PermissionDenied) when
// the caller may not make it for the tenant it names.
func (g *guardedStream) Receive(msg any) error {
	if err := g.ServerStream.Receive(msg); err != nil {
		return err
	}
	request, ok := msg.(proto.Message)
	if !ok {
		return fault.New(fault.PermissionDenied, "this call is not open to callers")
	}
	g.call.tenant = auth.Tenant(request.ProtoReflect())
	return g.call.id.Authorize(g.call.access, g.call.tenant)
}
