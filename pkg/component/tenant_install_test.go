package component_test

import (
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// policies is a tenant component with a values file, a schema and a
// platform value with a placeholder, requiring the platform's engine.
func policies() component.PlatformEntry {
	return component.PlatformEntry{
		Definition: component.Definition{Name: "policies", Valid: true, Management: component.Tenant, Requires: []string{"engine"},
			UserValues:     "mode: Audit\nlevel: baseline\n",
			PlatformValues: component.Values{"cluster": "${cluster.name}", "level": "locked"},
		},
		Install: component.Install{Namespace: "engine", Main: component.Chart{Repository: "https://charts.example.com", Name: "policies"}},
	}
}

func tenantInput(pins map[string]component.Pin) component.PlatformInput {
	return component.PlatformInput{
		Release: "r1", Pins: pins,
		Cluster: component.Cluster{Name: "prod", Running: true, KubernetesMinor: "1.37"},
		Vars: func(string) component.Vars {
			return component.Vars{"cluster.name": "prod"}
		},
		Plan: crossplaneaws.PlatformPlan{Namespace: "tenant-acme", ClusterPrefix: "prod-1a2b"},
	}
}

func TestPlanTenant(t *testing.T) {
	pinned := map[string]component.Pin{"policies": {Version: "3.10.0", Values: component.Values{"replicas": int64(2)}}}
	tests := []struct {
		name       string
		pins       map[string]component.Pin
		tenant     component.Values
		ready      map[string]string // required components installed elsewhere, with their last Release object
		wantReason string
		wantValues component.Values
	}{
		{name: "the tenant's values over the file, the platform's over both, placeholders filled in the platform's only",
			pins: pinned, tenant: component.Values{"mode": "Enforce", "note": "${cluster.name}"},
			ready:      map[string]string{"engine": "prod-1a2b-engine"},
			wantValues: component.Values{"mode": "Enforce", "level": "locked", "note": "${cluster.name}", "cluster": "prod", "replicas": int64(2)}},
		{name: "no tenant values: the file's", pins: pinned, ready: map[string]string{"engine": "prod-1a2b-engine"},
			wantValues: component.Values{"mode": "Audit", "level": "locked", "cluster": "prod", "replicas": int64(2)}},
		{name: "a required component that is not installed", pins: pinned, ready: map[string]string{},
			wantReason: "requires engine, which is not installed"},
		{name: "not in the release", pins: map[string]component.Pin{}, ready: map[string]string{"engine": "x"},
			wantReason: "not in release r1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tenantInput(tt.pins)

			plan, reason := component.PlanTenant(in, policies(), tt.tenant, tt.ready)

			if tt.wantReason != "" {
				if !strings.Contains(reason, tt.wantReason) {
					t.Fatalf("reason %q, want %q", reason, tt.wantReason)
				}
				return
			}
			if reason != "" || len(plan.Components) != 1 {
				t.Fatalf("plan %+v, reason %q", plan, reason)
			}
			c := plan.Components[0]
			if !reflect.DeepEqual(c.Main.Values, tt.wantValues) {
				t.Errorf("values %v, want %v", c.Main.Values, tt.wantValues)
			}
			if c.Main.Chart.Version != "3.10.0" || plan.External["engine"] != tt.ready["engine"] {
				t.Errorf("chart %+v, external %v", c.Main.Chart, plan.External)
			}
		})
	}
}

// The tenant's values are never changed by a plan.
func TestPlanTenantChangesNoInput(t *testing.T) {
	tenant := component.Values{"mode": "Enforce"}
	e := policies()

	_, _ = component.PlanTenant(tenantInput(map[string]component.Pin{"policies": {Version: "1"}}), e, tenant,
		map[string]string{"engine": "x"})

	if len(tenant) != 1 || tenant["mode"] != "Enforce" || e.PlatformValues["cluster"] != "${cluster.name}" {
		t.Fatalf("inputs changed: %v, %v", tenant, e.PlatformValues)
	}
}

func TestTenantEntries(t *testing.T) {
	defs := catalogForTenant()

	entries := component.TenantEntries(defs)

	if len(entries) != 1 || entries[0].Name != "policies" || entries[0].Install.Main.Name != "policies" {
		t.Fatalf("entries %+v, want only the tenant component policies", entries)
	}
}

// LastRelease is the Helm release a component's install ends with: its
// last pinned companion, else its main chart.
func TestLastRelease(t *testing.T) {
	e := policies()
	e.Install.Companions = []component.Companion{{Name: "policies-extra"}}
	tests := []struct {
		name string
		pins map[string]component.Pin
		want string
	}{
		{"companion pinned", map[string]component.Pin{"policies": {}, "policies-extra": {}}, "policies-extra"},
		{"companion not pinned", map[string]component.Pin{"policies": {}}, "policies"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := component.LastRelease(e, tt.pins); got != tt.want {
				t.Errorf("LastRelease = %q, want %q", got, tt.want)
			}
		})
	}
}

// catalogForTenant is a platform component, a tenant one, and an internal
// tenant one (a bundle member): only the second is a tenant entry.
func catalogForTenant() []cloudv1alpha1.ComponentDefinition {
	def := func(name string, management cloudv1alpha1.ComponentManagement, internal bool) cloudv1alpha1.ComponentDefinition {
		return cloudv1alpha1.ComponentDefinition{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudv1alpha1.ComponentDefinitionSpec{Namespace: "ns", Management: management, Internal: internal,
				Install: &cloudv1alpha1.ComponentInstall{Helm: &cloudv1alpha1.HelmInstall{
					HelmChartSource: cloudv1alpha1.HelmChartSource{Repository: "https://charts.example.com", Chart: name}}}}}
	}
	return []cloudv1alpha1.ComponentDefinition{
		def("engine", cloudv1alpha1.ComponentManagementPlatform, false),
		def("policies", cloudv1alpha1.ComponentManagementTenant, false),
		def("member", cloudv1alpha1.ComponentManagementTenant, true),
	}
}
