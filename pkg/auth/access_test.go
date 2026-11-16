package auth_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/yindia/cluster-vending/pkg/auth"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// methods are all RPCs of the cloud.v1 package.
func methods(t *testing.T) []protoreflect.MethodDescriptor {
	t.Helper()
	var out []protoreflect.MethodDescriptor
	protoregistry.GlobalFiles.RangeFilesByPackage("cloud.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			service := fd.Services().Get(i)
			for j := range service.Methods().Len() {
				out = append(out, service.Methods().Get(j))
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatal("no cloud.v1 services registered")
	}
	return out
}

// Every RPC of the API is classified: a new one that neither carries a
// tenant_id nor is listed in the scope overrides fails here, before it can
// be served open or refused by mistake.
func TestEveryRPCIsClassified(t *testing.T) {
	want := map[protoreflect.FullName]auth.Access{
		"cloud.v1.AdminService.GetFreeze":              {Scope: auth.ScopePlatform},
		"cloud.v1.AdminService.Freeze":                 {Scope: auth.ScopePlatform, Write: true},
		"cloud.v1.AdminService.Unfreeze":               {Scope: auth.ScopePlatform, Write: true},
		"cloud.v1.AdminService.ListReleases":           {Scope: auth.ScopePlatform},
		"cloud.v1.AdminService.GetRelease":             {Scope: auth.ScopePlatform},
		"cloud.v1.AdminService.ListReleaseChannels":    {Scope: auth.ScopePlatform},
		"cloud.v1.AdminService.GetReleaseChannel":      {Scope: auth.ScopePlatform},
		"cloud.v1.ComponentService.GetComponentValues": {Scope: auth.ScopeAuthenticated},
		"cloud.v1.TenantService.WhoAmI":                {Scope: auth.ScopeAuthenticated},
		"cloud.v1.TenantService.ListTenants":           {Scope: auth.ScopeAuthenticated},
		"cloud.v1.TenantService.CreateTenant":          {Scope: auth.ScopeTenant, Write: true},
		"cloud.v1.TenantService.BootstrapTenant":       {Scope: auth.ScopeTenant, Write: true},
		"cloud.v1.TenantService.GetCloudAccount":       {Scope: auth.ScopeTenant},
		"cloud.v1.TenantService.VerifyCloudAccount":    {Scope: auth.ScopeTenant, Write: true},
		"cloud.v1.TenantService.DeleteCloudAccount":    {Scope: auth.ScopeTenant, Write: true},
		"cloud.v1.TenantService.GetAccountOnboarding":  {Scope: auth.ScopeTenant},
		"cloud.v1.TenantService.Freeze":                {Scope: auth.ScopeTenant, Write: true},
		"cloud.v1.TenantService.Unfreeze":              {Scope: auth.ScopeTenant, Write: true},
		"cloud.v1.TenantService.GetFreeze":             {Scope: auth.ScopeTenant},
		"cloud.v1.TenantService.ListReleases":          {Scope: auth.ScopeTenant},
		"cloud.v1.TenantService.GetRelease":            {Scope: auth.ScopeTenant},
		"cloud.v1.TenantService.ListReleaseChannels":   {Scope: auth.ScopeTenant},
		"cloud.v1.TenantService.GetReleaseChannel":     {Scope: auth.ScopeTenant},
		"cloud.v1.ClusterService.CreateCluster":        {Scope: auth.ScopeTenant, Write: true},
		"cloud.v1.ClusterService.DeleteCluster":        {Scope: auth.ScopeTenant, Write: true},
		"cloud.v1.ClusterService.RetryCluster":         {Scope: auth.ScopeTenant, Write: true},
		"cloud.v1.ClusterService.GetCluster":           {Scope: auth.ScopeTenant},
		"cloud.v1.NetworkService.UpdateNetwork":        {Scope: auth.ScopeTenant, Write: true},
		"cloud.v1.NetworkService.ListNetworks":         {Scope: auth.ScopeTenant},
		"cloud.v1.ComponentService.EnableComponent":    {Scope: auth.ScopeTenant, Write: true},
	}
	seen := map[protoreflect.FullName]bool{}
	for _, m := range methods(t) {
		got, err := auth.Classify(m)

		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		seen[m.FullName()] = true
		if w, ok := want[m.FullName()]; ok && got != w {
			t.Errorf("%s = %+v, want %+v", m.FullName(), got, w)
		}
		if got.Scope == auth.ScopeTenant && m.Input().Fields().ByName(auth.TenantField) == nil {
			t.Errorf("%s is a tenant RPC without %s", m.FullName(), auth.TenantField)
		}
		if strings.HasPrefix(string(m.FullName()), "cloud.v1.AdminService.") && got.Scope != auth.ScopePlatform {
			t.Errorf("%s is an operator's call, got scope %s", m.FullName(), got.Scope)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("%s is expected but is not an RPC of the API", name)
		}
	}
}

// A request without a tenant_id that is not listed is refused, not open.
func TestClassifyRefusesAnUnlistedRPCWithoutATenant(t *testing.T) {
	// The RPCs without a tenant_id are all listed; under a name that is
	// not, each stands for a new RPC somebody forgot to classify.
	for _, m := range methods(t) {
		if m.Input().Fields().ByName(auth.TenantField) != nil {
			continue
		}
		if _, err := auth.Classify(renamed{m}); err == nil || !strings.Contains(err.Error(), "not classified") {
			t.Errorf("%s under another name: %v, want a refusal", m.FullName(), err)
		}
	}
}

// renamed is a method under a name the overrides do not list.
type renamed struct{ protoreflect.MethodDescriptor }

func (r renamed) FullName() protoreflect.FullName { return "cloud.v1.NewService.NewMethod" }

func TestTenantOfARequest(t *testing.T) {
	if got := auth.Tenant((&cloudv1.GetClusterRequest{TenantId: "acme"}).ProtoReflect()); got != "acme" {
		t.Errorf("Tenant = %q, want acme", got)
	}
	if got := auth.Tenant((&cloudv1.GetComponentValuesRequest{Name: "x"}).ProtoReflect()); got != "" {
		t.Errorf("a request without tenant_id = %q", got)
	}
}

func TestScopeString(t *testing.T) {
	for scope, want := range map[auth.Scope]string{auth.ScopeTenant: "tenant", auth.ScopePlatform: "platform",
		auth.ScopeAuthenticated: "authenticated", 0: "unclassified"} {
		if scope.String() != want {
			t.Errorf("%d = %s, want %s", scope, scope, want)
		}
	}
}
