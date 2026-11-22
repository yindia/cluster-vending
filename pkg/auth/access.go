package auth

import (
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// What a request says about itself, for the audit log (pkg/server/authn).
// Nothing here decides whether a call is allowed: an authenticated caller
// may call every RPC.

// TenantField is the request field that names the tenant of a call.
const TenantField = "tenant_id"

// Writes reports whether the RPC method has side effects: it is not marked
// with idempotency_level NO_SIDE_EFFECTS.
func Writes(method protoreflect.MethodDescriptor) bool {
	options, ok := method.Options().(*descriptorpb.MethodOptions)
	return !ok || options.GetIdempotencyLevel() != descriptorpb.MethodOptions_NO_SIDE_EFFECTS
}

// Tenant is the tenant a request names in its tenant_id; "" when it has
// none.
func Tenant(request protoreflect.Message) string {
	field := request.Descriptor().Fields().ByName(TenantField)
	if field == nil || field.Kind() != protoreflect.StringKind || field.IsList() {
		return ""
	}
	return request.Get(field).String()
}
