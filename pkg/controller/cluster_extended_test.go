package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// extendedRelease offers 1.36 alpha, 1.35 beta, 1.34 stable and 1.33 (the
// hand-made cluster's minor) on no channel.
type extendedRelease struct{}

func (extendedRelease) Offered(context.Context, string) (string, error) { return "extended", nil }

func (extendedRelease) Release(_ context.Context, name string) (*cloudv1alpha1.Release, error) {
	if name != "extended" {
		return nil, nil
	}
	return &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{Kubernetes: []cloudv1alpha1.KubernetesRelease{
			{Minor: "1.36", Channel: "alpha", Default: true}, {Minor: "1.35", Channel: "beta"},
			{Minor: "1.34", Channel: "stable"}, {Minor: "1.33", UpgradeTo: "1.34"},
		}}}}, nil
}

// kubectl cannot do what the API refuses: an edit making a cluster on an
// extended-support minor automatic, or STANDARD, is not applied and says
// why; corrected, it goes.
func TestKubectlEditOnAnExtendedMinorIsRefused(t *testing.T) {
	w := newWorld(t)
	w.deps.Releases = extendedRelease{}
	r, id := w.settledCluster(t)
	before, err := w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	refused := func(want string) {
		t.Helper()
		_, err := r.Reconcile(t.Context(), request("prod"))
		w.must(t, err)
		cond := meta.FindStatusCondition(w.cluster(t, "prod").Status.Conditions, ConditionEditRefused)
		if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, want) {
			t.Fatalf("condition %v, want EditRefused %q", cond, want)
		}
		row, err := w.store.Clusters().Get(t.Context(), tenant, id)
		w.must(t, err)
		if row.Generation != before.Generation {
			t.Fatalf("generation %d (was %d): the refused edit was applied", row.Generation, before.Generation)
		}
	}

	w.edit(t, 2, func(o *cloudv1alpha1.Cluster) { o.Spec.UpgradeMode = cloudv1alpha1.UpgradeModeAutomatic })
	refused("Kubernetes 1.33 is an extended-support version (the release offers it on no channel): it runs with upgradeMode Manual only; upgrade to 1.34 first (or keep Manual)")

	w.edit(t, 3, func(o *cloudv1alpha1.Cluster) {
		o.Spec.UpgradeMode = cloudv1alpha1.UpgradeModeManual
		o.Spec.AWS.UpgradePolicy = &cloudv1alpha1.AWSUpgradePolicy{SupportType: "STANDARD"}
	})
	refused("AWS_SUPPORT_TYPE_STANDARD would have AWS upgrade the control plane by force")

	w.edit(t, 4, func(o *cloudv1alpha1.Cluster) { o.Spec.AWS.UpgradePolicy = nil })
	_, err = r.Reconcile(t.Context(), request("prod"))
	w.must(t, err)
	if cond := meta.FindStatusCondition(w.cluster(t, "prod").Status.Conditions, ConditionEditRefused); cond != nil {
		t.Fatalf("corrected edit: condition %v is still there", cond)
	}
	row, err := w.store.Clusters().Get(t.Context(), tenant, id)
	w.must(t, err)
	if row.Generation != before.Generation+1 {
		t.Fatalf("corrected edit: generation %d, want %d", row.Generation, before.Generation+1)
	}
}
