package cli

import (
	"context"
	"fmt"
	"net/url"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// TokenSource is the bearer token to call the API with; "" for none
// (login.Source.Token).
type TokenSource func(ctx context.Context) (string, error)

// ClientOption adjusts NewClients.
type ClientOption func(*clientOptions)

type clientOptions struct{ token TokenSource }

// WithToken sends the source's token with every call, as "Authorization:
// Bearer". A source without a token sends none: the call is made
// unauthenticated, and a server that needs a token says so.
func WithToken(source TokenSource) ClientOption {
	return func(o *clientOptions) { o.token = source }
}

// bearerToken is the client interceptor behind WithToken. It refuses to
// send a token to a server reached over plain http, unless that server is
// this machine: the token would cross the network readable.
func bearerToken(server string, source TokenSource) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			token, err := source(ctx)
			if err != nil {
				return nil, err
			}
			if token == "" {
				return next(ctx, spec)
			}
			if !sendsTokensSafely(server) {
				return nil, fmt.Errorf("refusing to send your token to %s over plain http: use an https --server", server)
			}
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				info.RequestHeader().Set("Authorization", "Bearer "+token)
			}
			return next(ctx, spec)
		}
	}
}

// sendsTokensSafely reports whether server is reached over https, or is
// this machine.
func sendsTokensSafely(server string) bool {
	u, err := url.Parse(server)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return u.Scheme == "https"
}

// WhoAmI shows the caller as the server sees it (TenantService.WhoAmI):
// who the token stands for and what the server's rules grant.
func WhoAmI(ctx context.Context, c Clients, _ Options) (proto.Message, error) {
	return call(ctx, &cloudv1.WhoAmIRequest{}, c.Tenant.WhoAmI)
}
