package management

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/rollout"
)

// The management cluster's platform source gates code changes.
var _ reconcile.CodeAdmitter = platformSource{}

func TestCodeAdmittedBy(t *testing.T) {
	planned := &cloudv1alpha1.ChannelReleaseMoves{Admitted: []string{"tenant-acme/prod"}, Waiting: "tier 2 first"}
	tests := []struct {
		name string
		m    *cloudv1alpha1.ChannelReleaseMoves
		key  string
		want bool
		why  string
	}{
		{"admitted", planned, "tenant-acme/prod", true, ""},
		{"waiting", planned, "tenant-acme/dev", false, "tier 2 first"},
		{"not planned", nil, "tenant-acme/prod", false, "to be planned"},
		{"halted", &cloudv1alpha1.ChannelReleaseMoves{Halted: true, Waiting: "2 failed"}, "tenant-acme/prod", false, "halted: 2 failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := codeAdmittedBy(tt.m, tt.key)
			if got != tt.want || !strings.Contains(why, tt.why) {
				t.Errorf("= %v %q, want %v %q", got, why, tt.want, tt.why)
			}
		})
	}
}

// A network's code change is admitted once each channel of its clusters
// reached the network's tier, and not while the platform is frozen.
func TestNetworkCodeAdmittedReadsTheManagementCluster(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	ns := kube.TenantNamespace("acme")
	network := &cloudv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "main",
		Labels: map[string]string{cloudstore.LabelID: "net-1"}}}
	prod := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "prod", Labels: map[string]string{cloudstore.LabelID: "c-1"}},
		Spec: cloudv1alpha1.ClusterSpec{ReleaseChannel: cloudv1alpha1.ReleaseChannelStable}}
	prod.Spec.Network.NetworkRef.Name = "main"
	other := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "other"},
		Spec: cloudv1alpha1.ClusterSpec{ReleaseChannel: cloudv1alpha1.ReleaseChannelBeta}}
	other.Spec.Network.NetworkRef.Name = "elsewhere"
	stable := &cloudv1alpha1.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: "stable"},
		Status: cloudv1alpha1.ReleaseChannelStatus{CodeChanges: &cloudv1alpha1.ChannelReleaseMoves{Tier: cloudv1alpha1.ClusterTier1}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(network, prod, other, stable).WithStatusSubresource(stable).Build()
	p := platformSource{c: c}

	got, why, err := p.NetworkCodeAdmitted(t.Context(), "acme", "net-1")
	if err != nil || got || !strings.Contains(why, "reach tier 0") {
		t.Fatalf("tier 1 rolling: %v %q %v; want waiting for tier 0 (prod's)", got, why, err)
	}

	stable.Status.CodeChanges.Tier = cloudv1alpha1.ClusterTier0
	stable.Status.CodeChanges.Admitted = []string{rollout.NetworkCodeKey(network)}
	if err := c.Status().Update(t.Context(), stable); err != nil {
		t.Fatal(err)
	}
	if got, why, err := p.NetworkCodeAdmitted(t.Context(), "acme", "net-1"); err != nil || !got {
		t.Fatalf("tier 0 rolling: %v %q %v; want admitted", got, why, err)
	}

	hold := &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: "platform-freeze"},
		Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "deploy"}}
	if err := c.Create(t.Context(), hold); err != nil {
		t.Fatal(err)
	}
	if got, why, err := p.NetworkCodeAdmitted(t.Context(), "acme", "net-1"); err != nil || got || !strings.Contains(why, "frozen") {
		t.Fatalf("frozen: %v %q %v", got, why, err)
	}
}
