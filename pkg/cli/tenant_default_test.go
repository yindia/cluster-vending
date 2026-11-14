package cli

import (
	"strings"
	"testing"
)

// CV_TENANT is the tenant when neither --tenant nor a manifest names one:
// the flag wins, then metadata.tenant, then the default.
func TestDefaultTenant(t *testing.T) {
	tests := []struct {
		name, flag, def, manifest, want, wantErr string
	}{
		{name: "the default when nothing else names one", def: "acme", want: "acme"},
		{name: "the flag beats the default", flag: "globex", def: "acme", want: "globex"},
		{name: "the manifest beats the default", def: "acme", manifest: "demo", want: "demo"},
		{name: "the manifest without a default", manifest: "demo", want: "demo"},
		{name: "flag and manifest still must agree", flag: "globex", manifest: "demo", wantErr: "differ"},
		{name: "nothing names one", wantErr: "the tenant is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := Options{Tenant: tt.flag, DefaultTenant: tt.def}.withDefaultTenant()
			m := &Manifest{}
			m.Metadata.Tenant = tt.manifest

			got, err := m.tenantFor(o)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("tenant = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// A command without a manifest takes the default too.
func TestDefaultTenantWithoutManifest(t *testing.T) {
	if err := (Options{DefaultTenant: "acme"}.withDefaultTenant()).requireTenant(); err != nil {
		t.Fatalf("requireTenant with CV_TENANT: %v", err)
	}
	err := Options{}.withDefaultTenant().requireTenant()
	if err == nil || !strings.Contains(err.Error(), EnvTenant) {
		t.Fatalf("err = %v, want it to name %s", err, EnvTenant)
	}
}
