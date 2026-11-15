package cli

import (
	"bytes"
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestRetryHint(t *testing.T) {
	failed := cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED
	tests := []struct {
		name, chart, message string
		phase                cloudv1.ComponentPhase
		want                 string
	}{
		{"a failed chart: the command", "kyverno", "chart kyverno: create failed", failed,
			"retry: cluster-vending cluster retry --id c1 --component kyverno"},
		{"a precondition: what to fix", "", "component karpenter uses IRSA, which cluster demo has off", failed,
			"fix first, then it installs by itself: component karpenter uses IRSA"},
		{"ready: nothing", "istiod", "", cloudv1.ComponentPhase_COMPONENT_PHASE_READY, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := retryHint("c1", tt.name, tt.phase, tt.chart, tt.message)
			if tt.want == "" && got != "" || !strings.Contains(got, strings.ReplaceAll(tt.want, "kyverno", tt.name)) && !strings.Contains(got, tt.want) {
				t.Errorf("hint = %q, want %q", got, tt.want)
			}
		})
	}
}

// component list --details says how to recover a failed component.
func TestComponentDetailsSayHowToRecover(t *testing.T) {
	var out bytes.Buffer
	comps := []*cloudv1.Component{{Component: "kyverno", ClusterId: "c1", Status: &cloudv1.ComponentStatus{
		Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Chart: "kyverno", HelmState: "failed", LastError: "connection reset by peer"}}}

	if err := printDetails(&out, comps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "retry: cluster-vending cluster retry --id c1 --component kyverno") {
		t.Fatalf("details:\n%s", out.String())
	}
}

// cluster get lists, under the platform components, how to recover the
// failed ones.
func TestClusterGetSaysHowToRecover(t *testing.T) {
	c := testCluster()
	c.Status.PlatformComponents = []*cloudv1.PlatformComponent{
		{Name: "kyverno", Version: "3.10.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Chart: "kyverno", Message: "chart kyverno: create failed"},
		{Name: "karpenter", Version: "1.15.1", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED,
			Message: "component karpenter uses IRSA, which cluster prod-eks has off: set spec.aws.irsa to true on the cluster first"},
	}

	got := printed(t, &cloudv1.GetClusterResponse{Cluster: c}, OutputTable)

	for _, want := range []string{"\nRecovery:\n",
		"kyverno: retry: cluster-vending cluster retry --id " + c.GetId() + " --component kyverno",
		"karpenter: fix first, then it installs by itself: component karpenter uses IRSA"} {
		if !strings.Contains(got, want) {
			t.Errorf("cluster get lacks %q:\n%s", want, got)
		}
	}
}
