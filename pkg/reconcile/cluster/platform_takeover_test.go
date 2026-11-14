package cluster

import (
	"strings"
	"testing"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// An imported cluster gets the platform components once it is taken
// over, like any cluster: not before, and not when its takeover asked to
// keep them off.
func TestPlatformComponentsFollowTheTakeover(t *testing.T) {
	skipped := func(r *model.Cluster) {
		raw, err := WithoutPlatformComponents(r.Adoption)
		if err != nil {
			panic(err)
		}
		r.Adoption = raw
	}
	tests := []struct {
		name string
		edit func(*model.Cluster)
		want string // phases; "-" for no components
	}{
		{"imported, not taken over", func(r *model.Cluster) { r.Imported, r.State = true, StateImported }, "-"},
		{"an import that is blocked", func(r *model.Cluster) { r.Imported, r.State = true, StateBlocked }, "-"},
		{"taken over", func(r *model.Cluster) { r.Imported = true }, "base=INSTALLING,mesh=PENDING"},
		{"taken over, platform components kept off", func(r *model.Cluster) { r.Imported = true; skipped(r) }, "-"},
		{"created by the platform", func(*model.Cluster) {}, "base=INSTALLING,mesh=PENDING"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, _ := platformWorld(t)
			w.helmConfig(t)
			row := w.cluster(t)
			tt.edit(row)

			p, err := w.svc.installPlatform(t.Context(), row)

			if err != nil {
				t.Fatal(err)
			}
			got := "-"
			if p != nil && len(p.components) > 0 {
				got = phases(p)
			}
			if got != tt.want {
				t.Errorf("components = %s, want %s", got, tt.want)
			}
		})
	}
}

// A taken-over cluster keeps the IAM OIDC provider it was imported with
// (a carried-over object) while spec.aws.irsa is off: it has IRSA, and a
// component with an IRSA role is installed, not refused.
func TestTakenOverClusterWithItsOwnOIDCProviderHasIRSA(t *testing.T) {
	irsa := platformDef("base")
	irsa.Spec.CloudResources = &cloudv1alpha1.ComponentCloudResources{IAMRole: &cloudv1alpha1.ComponentIAMRole{
		ServiceAccounts:   []cloudv1alpha1.ComponentServiceAccount{{Namespace: "base-system", Name: "base"}},
		ManagedPolicyARNs: []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"}}}
	tests := []struct {
		name     string
		provider bool
		refused  bool
	}{
		{"no provider, irsa off", false, true},
		{"the provider it was imported with", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, src := platformWorld(t)
			src.defs[1] = irsa
			w.helmConfig(t)
			row := w.cluster(t)
			row.Imported = true
			if tt.provider {
				if err := w.store.ManagedResources().Put(t.Context(), []model.ManagedResource{{TenantID: worldTenant,
					CloudAccountID: row.CloudAccountID, OwnerKind: model.OwnerCluster, OwnerID: row.ID,
					APIVersion: "iam.aws.m.upbound.io/v1beta1", Kind: "OpenIDConnectProvider", Namespace: worldNS,
					Name: "tf-oidc", CarriedOver: true}}); err != nil {
					t.Fatal(err)
				}
			}

			p, err := w.svc.installPlatform(t.Context(), row)

			if err != nil {
				t.Fatal(err)
			}
			var base *cloudv1.PlatformComponent
			for _, c := range p.components {
				if c.GetName() == "base" {
					base = c
				}
			}
			if base == nil {
				t.Fatalf("no base component in %s", phases(p))
			}
			if refused := strings.Contains(base.GetMessage(), "IRSA"); refused != tt.refused {
				t.Errorf("base: %s %q; refused for IRSA = %v, want %v", base.GetPhase(), base.GetMessage(), refused, tt.refused)
			}
		})
	}
}

// A component that fails on a taken-over cluster says what is special
// about such a cluster: it may already run the component itself.
func TestFailedComponentOnATakenOverClusterSaysWhy(t *testing.T) {
	failed := func() []*cloudv1.PlatformComponent {
		return []*cloudv1.PlatformComponent{
			{Name: "karpenter", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Message: "chart karpenter: create failed: cannot re-use a name that is still in use"},
			{Name: "kyverno", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY},
		}
	}
	tests := []struct {
		name     string
		imported bool
		hint     bool
	}{
		{"taken over", true, true},
		{"created by the platform", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			components := failed()

			explainTakenOver(&model.Cluster{Imported: tt.imported, Name: "tf"}, components)

			if got := strings.Contains(components[0].GetMessage(), "already run"); got != tt.hint {
				t.Errorf("failed component: %q; hint = %v, want %v", components[0].GetMessage(), got, tt.hint)
			}
			if !strings.Contains(components[0].GetMessage(), "cannot re-use a name") {
				t.Errorf("the provider's error is gone: %q", components[0].GetMessage())
			}
			if components[1].GetMessage() != "" {
				t.Errorf("a ready component got a message: %q", components[1].GetMessage())
			}
		})
	}
}
