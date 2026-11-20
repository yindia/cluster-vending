package cluster

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/catalog"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func shippedPinOn(t *testing.T, minor string) string {
	t.Helper()
	rel, err := catalog.ShippedRelease()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range rel.Kubernetes {
		if k.Minor == minor {
			return k.NodeAMIPin
		}
	}
	t.Fatalf("shipped release has no minor %s", minor)
	return ""
}

// A cluster recorded on a release published before nodeAmiPin existed does
// not follow the newest AMI: its node classes pin what this build ships.
func TestAMIPinFallsBackToTheShippedPinNotLatest(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row, src := nodeGroupWorldSrc(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})
	iamFake{role: true, access: true, policy: true, attachments: 4, passRoleOn: nodeRoleARN(row)}.seed(t, w, row, "us-east-1")
	src.release.Spec.Content.Kubernetes[0].NodeAMIPin = "" // the recorded release has none
	want := shippedPinOn(t, "1.37")

	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	class := fw.get(t, "EC2NodeClass", "", "gpu")
	terms, _, _ := unstructured.NestedSlice(class.Object, "spec", "amiSelectorTerms")
	if got := terms[0].(map[string]any)["alias"]; got != "al2023@"+want || strings.HasSuffix(got.(string), "@latest") {
		t.Errorf("AMI alias %v, want al2023@%s", got, want)
	}
}

// The release's own pin still wins over the shipped one.
func TestAMIPinOfTheRecordedReleaseWins(t *testing.T) {
	rel := &cloudv1alpha1.Release{}
	rel.Spec.Content.Kubernetes = []cloudv1alpha1.KubernetesRelease{{Minor: "1.37", NodeAMIPin: "v20200101"}}
	if pin, notice := resolveAMIPin(rel, "1.37"); pin != "v20200101" || notice != "" {
		t.Errorf("pin %q notice %q", pin, notice)
	}
}

// With neither the release nor this build pinning the minor the pin is
// latest, and the status says so.
func TestAMIPinLatestOnlyWhenNothingPinsAndSaysSo(t *testing.T) {
	rel := &cloudv1alpha1.Release{}
	pin, notice := resolveAMIPin(rel, "1.99")
	if pin != "" || !strings.Contains(notice, "latest") || !strings.Contains(notice, "1.99") {
		t.Errorf("pin %q notice %q", pin, notice)
	}
	if pin, notice := resolveAMIPin(nil, "1.37"); pin != shippedPinOn(t, "1.37") || notice != "" {
		t.Errorf("no release: pin %q notice %q", pin, notice)
	}
}

// The notice reaches the user groups' status.
func TestAMIPinNoticeIsOnTheUserGroups(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row, src := nodeGroupWorldSrc(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})
	iamFake{role: true, access: true, policy: true, attachments: 4, passRoleOn: nodeRoleARN(row)}.seed(t, w, row, "us-east-1")
	src.release.Spec.Content.Kubernetes[0].NodeAMIPin = ""
	src.release.Spec.Content.Kubernetes[0].Minor = "1.99"
	spec, _ := SpecOf(row)
	spec.ControlPlane.KubernetesVersion = "1.99"
	row.Spec, _ = Encode(spec)

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	if g := statusOf(res, "gpu"); g == nil || !strings.Contains(g.GetMessage(), "latest") {
		t.Errorf("gpu status = %v", g)
	}
}
