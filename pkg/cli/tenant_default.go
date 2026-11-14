package cli

// EnvTenant names the tenant a command acts for when neither --tenant nor
// a manifest's metadata.tenant does, so it is set once per shell.
const EnvTenant = "CV_TENANT"

// withDefaultTenant is o with DefaultTenant (CV_TENANT) as its tenant when
// --tenant was not given; a manifest's metadata.tenant still wins over it
// (tenantFor).
func (o Options) withDefaultTenant() Options {
	if o.Tenant == "" && o.DefaultTenant != "" {
		o.Tenant, o.tenantDefaulted = o.DefaultTenant, true
	}
	return o
}

// tenantFor is the tenant a manifest is applied to: --tenant, which must
// agree with metadata.tenant; else metadata.tenant; else the default.
func (m *Manifest) tenantFor(o Options) (string, error) {
	if o.tenantDefaulted {
		if m.Metadata.Tenant != "" {
			return m.Metadata.Tenant, nil
		}
		return o.Tenant, nil
	}
	return m.tenant(o.Tenant)
}
