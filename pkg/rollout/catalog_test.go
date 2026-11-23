package rollout

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

func withCatalog(c cloudv1alpha1.Cluster, phase string) cloudv1alpha1.Cluster {
	c.Status.CatalogChange = &cloudv1alpha1.CatalogChange{Phase: phase, Summary: "1 catalog entry changes: kyverno (removed)"}
	return c
}

func catalogFinishedAgo(c cloudv1alpha1.Cluster, ago time.Duration) cloudv1alpha1.Cluster {
	at := metav1.NewTime(now.Add(-ago))
	c.Status.CatalogChange.FinishTime = &at
	return c
}

// A catalog change is a member of the code rollout (PROP-01 4.7): tier 2
// first, Manual clusters too, in progress against maxInProgress, frozen
// clusters blocked, the soak counted from its finish, and the rollout
// halted past maxFailed.
func TestComputeCodeAdmitsCatalogChanges(t *testing.T) {
	soaked, fresh := 2*time.Hour, 10*time.Minute
	manual := withCatalog(cluster("manual", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangePending)
	manual.Spec.UpgradeMode = cloudv1alpha1.UpgradeModeManual
	freeze := []cloudv1alpha1.ReleaseHold{{Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "deploy"}}}
	tests := []struct {
		name     string
		clusters []cloudv1alpha1.Cluster
		holds    []cloudv1alpha1.ReleaseHold
		admitted []string
		tier     cloudv1alpha1.ClusterTier
		halted   bool
		blocked  string
	}{
		{name: "tier 2 first, a Manual cluster too",
			clusters: []cloudv1alpha1.Cluster{withCatalog(cluster("dev", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangePending), manual,
				withCatalog(cluster("prod", cloudv1alpha1.ClusterTier0, "1.36"), convert.CodeChangePending)},
			admitted: []string{"tenant-acme/dev", "tenant-acme/manual"}, tier: cloudv1alpha1.ClusterTier2},
		{name: "one applying counts against maxInProgress",
			clusters: []cloudv1alpha1.Cluster{withCatalog(cluster("a", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangeApplying),
				withCatalog(cluster("b", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangePending),
				withCatalog(cluster("c", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangePending)},
			admitted: []string{"tenant-acme/b"}, tier: cloudv1alpha1.ClusterTier2},
		{name: "the next tier waits for the soak",
			clusters: []cloudv1alpha1.Cluster{catalogFinishedAgo(withCatalog(cluster("dev", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangeDone), fresh),
				withCatalog(cluster("staging", cloudv1alpha1.ClusterTier1, "1.36"), convert.CodeChangePending)},
			tier: cloudv1alpha1.ClusterTier1}, // tier 1 is next, after the soak: nothing admitted yet
		{name: "then goes",
			clusters: []cloudv1alpha1.Cluster{catalogFinishedAgo(withCatalog(cluster("dev", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangeDone), soaked),
				withCatalog(cluster("staging", cloudv1alpha1.ClusterTier1, "1.36"), convert.CodeChangePending)},
			admitted: []string{"tenant-acme/staging"}, tier: cloudv1alpha1.ClusterTier1},
		{name: "past maxFailed the rollout halts",
			clusters: []cloudv1alpha1.Cluster{withCatalog(cluster("a", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangeHalted),
				withCatalog(cluster("b", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangeHalted),
				withCatalog(cluster("c", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangePending)},
			halted: true},
		{name: "the platform freeze blocks it",
			clusters: []cloudv1alpha1.Cluster{withCatalog(cluster("dev", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangePending)},
			holds:    freeze, blocked: string(upgrade.GateFrozen)},
		{name: "a code change done does not hide a catalog change pending",
			clusters: []cloudv1alpha1.Cluster{withCatalog(withCode(cluster("dev", cloudv1alpha1.ClusterTier2, "1.36"), convert.CodeChangeDone), convert.CodeChangePending)},
			admitted: []string{"tenant-acme/dev"}, tier: cloudv1alpha1.ClusterTier2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := input(tt.clusters...)
			in.Holds = tt.holds

			got := ComputeCode(in)

			if !slices.Equal(got.Admitted, tt.admitted) || got.Halted != tt.halted || (tt.tier != "" && got.Tier != tt.tier) {
				t.Errorf("admitted %v tier %s halted %v; want %v %s %v (%+v)", got.Admitted, got.Tier, got.Halted, tt.admitted, tt.tier, tt.halted, got)
			}
			if tt.blocked != "" && (len(got.Blocked) == 0 || got.Blocked[0].Gate != tt.blocked) {
				t.Errorf("blocked %+v, want gate %s", got.Blocked, tt.blocked)
			}
		})
	}
}

// One member stands for both changes: the one further from done.
func TestClusterChange(t *testing.T) {
	early, late := metav1.NewTime(now.Add(-3*time.Hour)), metav1.NewTime(now.Add(-time.Hour))
	tests := []struct {
		name          string
		code, catalog string
		want          string
	}{
		{"neither", "", "", ""},
		{"code only", convert.CodeChangePending, "", convert.CodeChangePending},
		{"catalog only", "", convert.CodeChangeApplying, convert.CodeChangeApplying},
		{"applying wins", convert.CodeChangeHalted, convert.CodeChangeApplying, convert.CodeChangeApplying},
		{"halted over pending", convert.CodeChangePending, convert.CodeChangeHalted, convert.CodeChangeHalted},
		{"pending over done", convert.CodeChangeDone, convert.CodeChangePending, convert.CodeChangePending},
		{"both done", convert.CodeChangeDone, convert.CodeChangeDone, convert.CodeChangeDone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cluster("c", cloudv1alpha1.ClusterTier2, "1.36")
			if tt.code != "" {
				c.Status.CodeChange = &cloudv1alpha1.CodeChange{Phase: tt.code, FinishTime: &early}
			}
			if tt.catalog != "" {
				c.Status.CatalogChange = &cloudv1alpha1.CatalogChange{Phase: tt.catalog, FinishTime: &late}
			}
			got := clusterChange(&c)
			phase := ""
			if got != nil {
				phase = got.Phase
			}
			if phase != tt.want {
				t.Errorf("phase = %q, want %q", phase, tt.want)
			}
			if tt.want == convert.CodeChangeDone && tt.catalog != "" && !got.FinishTime.Equal(&late) {
				t.Errorf("finish = %v, want the later", got.FinishTime)
			}
		})
	}
}
