package rollout

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// onRelease is cluster on release rel, with no move under way.
func onRelease(name string, tier cloudv1alpha1.ClusterTier, rel string) cloudv1alpha1.Cluster {
	c := cluster(name, tier, "1.36")
	c.Status.PlatformRelease = rel
	return c
}

func moving(c cloudv1alpha1.Cluster, phase string, since time.Time) cloudv1alpha1.Cluster {
	st := metav1.NewTime(since)
	c.Status.ReleaseMove = &cloudv1alpha1.ClusterReleaseMove{Current: c.Status.PlatformRelease, Target: "shipped-00000001",
		Phase: phase, Component: "karpenter", StartTime: &st}
	return c
}

// Release moves go tier 2 first, at most maxInProgress at a time; a
// manual cluster, one on the release already and one with nothing
// installed are not started.
func TestComputeMovesAdmitsTierByTier(t *testing.T) {
	manual := onRelease("manual", cloudv1alpha1.ClusterTier2, "old")
	manual.Spec.UpgradeMode = cloudv1alpha1.UpgradeModeManual
	in := input(onRelease("a", cloudv1alpha1.ClusterTier2, "old"), onRelease("b", cloudv1alpha1.ClusterTier2, "old"),
		onRelease("c", cloudv1alpha1.ClusterTier2, "old"), onRelease("t1", cloudv1alpha1.ClusterTier1, "old"),
		onRelease("done", cloudv1alpha1.ClusterTier2, "shipped-00000001"), onRelease("fresh", cloudv1alpha1.ClusterTier2, ""), manual)

	m := ComputeMoves(in)

	if m.Release != "shipped-00000001" || !slices.Equal(m.Admitted, []string{"tenant-acme/a", "tenant-acme/b"}) {
		t.Fatalf("admitted %v (release %s, waiting %q)", m.Admitted, m.Release, m.Waiting)
	}
}

// A move under way counts against maxInProgress; a halted one is a
// failure, and more than maxFailed halt the rollout.
func TestComputeMovesInProgressAndHalt(t *testing.T) {
	in := input(moving(onRelease("a", cloudv1alpha1.ClusterTier2, "old"), "Components", now.Add(-time.Minute)),
		onRelease("b", cloudv1alpha1.ClusterTier2, "old"), onRelease("c", cloudv1alpha1.ClusterTier2, "old"))

	m := ComputeMoves(in)
	if !slices.Equal(m.Admitted, []string{"tenant-acme/b"}) {
		t.Fatalf("one in progress, maxInProgress 2: admitted %v", m.Admitted)
	}

	in = input(moving(onRelease("a", cloudv1alpha1.ClusterTier2, "old"), "Halted", now.Add(-time.Hour)),
		moving(onRelease("b", cloudv1alpha1.ClusterTier2, "old"), "Halted", now.Add(-time.Hour)),
		onRelease("c", cloudv1alpha1.ClusterTier2, "old"))
	m = ComputeMoves(in)
	if !m.Halted || len(m.Admitted) != 0 {
		t.Fatalf("two halted, maxFailed 1: halted %v, admitted %v", m.Halted, m.Admitted)
	}
}

// A cluster whose upgrades are paused, or that is upgrading Kubernetes, is
// blocked with the reason; it does not hold its tier back.
func TestComputeMovesBlocked(t *testing.T) {
	k8s := onRelease("k8s", cloudv1alpha1.ClusterTier2, "old")
	k8s.Status.Upgrade = &cloudv1alpha1.ClusterUpgrade{FromVersion: "1.35", ToVersion: "1.36", Phase: "ControlPlane"}
	paused := onRelease("paused", cloudv1alpha1.ClusterTier2, "old")
	paused.Annotations = map[string]string{"cloud.cluster-vending.io/paused": `{"by":"ops","since":"2026-11-14T00:00:00Z","reason":"x"}`}
	in := input(k8s, paused, onRelease("t1", cloudv1alpha1.ClusterTier1, "old"))

	m := ComputeMoves(in)

	if !slices.Equal(m.Admitted, []string{"tenant-acme/t1"}) {
		t.Fatalf("admitted %v, want the tier 1 cluster (tier 2's are blocked)", m.Admitted)
	}
	var why []string
	for _, b := range m.Blocked {
		why = append(why, b.Cluster+": "+b.Message)
	}
	if got := strings.Join(why, "; "); !strings.Contains(got, "k8s: ") || !strings.Contains(got, "Kubernetes") || !strings.Contains(got, "paused") {
		t.Errorf("blocked = %s", got)
	}
}

// A paused channel plans but admits no one.
func TestComputeMovesPausedChannel(t *testing.T) {
	in := input(onRelease("a", cloudv1alpha1.ClusterTier2, "old"))
	in.Channel.Spec.Paused = true

	m := ComputeMoves(in)

	if len(m.Admitted) != 0 || !strings.Contains(m.Waiting, "paused") {
		t.Fatalf("paused: admitted %v, waiting %q", m.Admitted, m.Waiting)
	}
}
