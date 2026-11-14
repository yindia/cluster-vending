package auth

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Scope is whose business an RPC is.
type Scope int

// Scopes.
const (
	// ScopeTenant: the call is about one tenant, named by the request's
	// tenant_id; the caller needs a role for that tenant.
	ScopeTenant Scope = iota + 1
	// ScopePlatform: an operator's call; the caller must be a platform
	// admin, whatever tenant the request names.
	ScopePlatform
	// ScopeAuthenticated: the same for every tenant (the catalog, who am
	// I); any accepted caller may make it.
	ScopeAuthenticated
)

// String names the scope the way docs and tests show it.
func (s Scope) String() string {
	switch s {
	case ScopeTenant:
		return "tenant"
	case ScopePlatform:
		return "platform"
	case ScopeAuthenticated:
		return "authenticated"
	default:
		return "unclassified"
	}
}

// Access is what an RPC needs from its caller.
type Access struct {
	Scope Scope
	// Write: the RPC is not marked NO_SIDE_EFFECTS, so a viewer may not
	// call it.
	Write bool
}

// TenantField is the request field that names the tenant of a tenant RPC.
const TenantField = "tenant_id"

// scopeOverrides are the RPCs whose scope the request does not show, by
// full method name. Every other RPC is a tenant RPC and must carry
// tenant_id: a new one that does not fails Classify (and the test that
// classifies every RPC) until it is listed here.
var scopeOverrides = map[protoreflect.FullName]Scope{
	// Operator actions on a tenant's resources.
	"cloud.v1.AdminService.PauseReconcile":  ScopePlatform,
	"cloud.v1.AdminService.ResumeReconcile": ScopePlatform,
	"cloud.v1.AdminService.ListPauses":      ScopePlatform,
	// The platform's release configuration and its rollouts.
	"cloud.v1.AdminService.ListReleases":        ScopePlatform,
	"cloud.v1.AdminService.GetRelease":          ScopePlatform,
	"cloud.v1.AdminService.ListReleaseChannels": ScopePlatform,
	"cloud.v1.AdminService.GetReleaseChannel":   ScopePlatform,
	"cloud.v1.AdminService.ListRollouts":        ScopePlatform,
	"cloud.v1.AdminService.GetRollout":          ScopePlatform,
	"cloud.v1.AdminService.ListReleaseHolds":    ScopePlatform,
	// The catalog's values file is the same for every tenant.
	"cloud.v1.ComponentService.GetComponentValues": ScopeAuthenticated,
	// The caller, as the server sees it.
	"cloud.v1.TenantService.WhoAmI": ScopeAuthenticated,
}

// Classify is what the RPC method needs: its scope from scopeOverrides or,
// for a request with a tenant_id, the tenant; read or write from its
// idempotency_level option. An RPC that is neither listed nor carries a
// tenant_id is an error: it is refused rather than left open.
func Classify(method protoreflect.MethodDescriptor) (Access, error) {
	access := Access{Write: !noSideEffects(method)}
	if scope, ok := scopeOverrides[method.FullName()]; ok {
		access.Scope = scope
		return access, nil
	}
	field := method.Input().Fields().ByName(TenantField)
	if field == nil || field.Kind() != protoreflect.StringKind || field.IsList() {
		return Access{}, fmt.Errorf("%s is not classified: its request has no %s and it is not in the scope overrides",
			method.FullName(), TenantField)
	}
	access.Scope = ScopeTenant
	return access, nil
}

// noSideEffects reports the method's idempotency_level NO_SIDE_EFFECTS.
func noSideEffects(method protoreflect.MethodDescriptor) bool {
	options, ok := method.Options().(*descriptorpb.MethodOptions)
	return ok && options.GetIdempotencyLevel() == descriptorpb.MethodOptions_NO_SIDE_EFFECTS
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
