package auth

import "context"

// Identity is the caller of a request.
type Identity struct {
	// Anonymous: the server does not authenticate (mode none). Every
	// caller, anonymous or not, may call every RPC for every tenant.
	Anonymous bool
	// Subject is the token's "sub".
	Subject string
	// Username is the issuer's username claim, for people to read.
	Username string
	// Issuer is the name of the Issuer that vouched for the caller.
	Issuer string
}

// Anonymous is the caller of a request without a token.
func Anonymous() *Identity { return &Identity{Anonymous: true} }

type identityKey struct{}

// WithIdentity returns ctx carrying the caller.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext is the caller ctx carries; anonymous when it carries none (a
// handler called without the interceptor).
func FromContext(ctx context.Context) *Identity {
	if id, ok := ctx.Value(identityKey{}).(*Identity); ok && id != nil {
		return id
	}
	return Anonymous()
}
