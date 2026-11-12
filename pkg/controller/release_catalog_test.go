package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// istioDefinitions are the three Istio entries of the catalog: one version
// group, one minor at a time.
func istioDefinitions() []client.Object {
	def := func(name string, requires ...string) client.Object {
		return &cloudv1alpha1.ComponentDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudv1alpha1.ComponentDefinitionSpec{Namespace: "istio-system", Requires: requires,
				VersionGroup: "istio", UpgradeOneMinorAtATime: true},
		}
	}
	return []client.Object{def("istio-base"), def("istiod", "istio-base"), def("istio-ingressgateway", "istiod")}
}

// withIstio is the shipped release with every minor pinning istio-base and
// istiod at base and istiod.
func withIstio(name, base, istiod string) *cloudv1alpha1.Release {
	rel := DefaultRelease()
	rel.Name = name
	for i := range rel.Spec.Content.Kubernetes {
		rel.Spec.Content.Kubernetes[i].Components = []cloudv1alpha1.ComponentPin{
			{Name: "istio-base", Version: base}, {Name: "istiod", Version: istiod}}
	}
	meta.SetStatusCondition(&rel.Status.Conditions, metav1.Condition{Type: cloudv1alpha1.ReleaseConditionValid,
		Status: metav1.ConditionTrue, Reason: "Valid"})
	return rel
}

// A release that pins a version group at two versions is not valid.
func TestReleaseRefusesASplitVersionGroup(t *testing.T) {
	tests := []struct {
		name      string
		release   *cloudv1alpha1.Release
		wantValid bool
	}{
		{"one version", withIstio("same", "1.32.0", "1.32.0"), true},
		{"split", withIstio("split", "1.32.0", "1.31.4"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := releaseClient(t, append(istioDefinitions(), tt.release)...)
			reconcileOnce(t, &ReleaseReconciler{c: c, render: crossplaneaws.RenderVersion}, tt.release.Name)
			rel := &cloudv1alpha1.Release{}
			if err := c.Get(t.Context(), client.ObjectKey{Name: tt.release.Name}, rel); err != nil {
				t.Fatal(err)
			}
			cond := meta.FindStatusCondition(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionValid)
			if got := cond.Status == metav1.ConditionTrue; got != tt.wantValid {
				t.Fatalf("Valid = %+v, want %v", cond, tt.wantValid)
			}
			if !tt.wantValid && !strings.Contains(cond.Message, "version group istio") {
				t.Errorf("message %q does not name the group", cond.Message)
			}
		})
	}
}

// A channel does not move to a release that skips a minor of a component
// that upgrades one minor at a time; it moves to one that does not.
func TestChannelRefusesSkippingAMinor(t *testing.T) {
	ctx := t.Context()
	current := withIstio("2026-11-a", "1.30.8", "1.30.8")
	skip := withIstio("2026-12-a", "1.32.0", "1.32.0")
	step := withIstio("2026-12-b", "1.31.4", "1.31.4")
	alpha := DefaultChannels()[0]
	alpha.Spec.Release = skip.Name
	alpha.Status.Release = current.Name
	c := releaseClient(t, append(istioDefinitions(), current, skip, step, alpha)...)
	r := &ReleaseChannelReconciler{c: c}

	reconcileOnce(t, r, "alpha")

	ch := &cloudv1alpha1.ReleaseChannel{}
	if err := c.Get(ctx, client.ObjectKey{Name: "alpha"}, ch); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(ch.Status.Conditions, cloudv1alpha1.ChannelConditionOffering)
	if ch.Status.Release != current.Name || cond.Status != metav1.ConditionFalse ||
		!strings.Contains(cond.Message, "skipping 1 minor version") || !strings.Contains(cond.Message, "still offers "+current.Name) {
		t.Fatalf("alpha offers %q with %+v, want it to stay on %s", ch.Status.Release, cond, current.Name)
	}

	ch.Spec.Release = step.Name
	if err := c.Update(ctx, ch); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "alpha")
	if err := c.Get(ctx, client.ObjectKey{Name: "alpha"}, ch); err != nil {
		t.Fatal(err)
	}
	if ch.Status.Release != step.Name {
		t.Errorf("alpha offers %q, want %s (one minor up)", ch.Status.Release, step.Name)
	}
}
