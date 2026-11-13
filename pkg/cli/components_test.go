package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func clusterWithComponents(components ...*cloudv1.PlatformComponent) *cloudv1.Cluster {
	return &cloudv1.Cluster{Id: kubeconfigCluster, Name: "prod-eks",
		Status: &cloudv1.ClusterStatus{PlatformRelease: "shipped-f874cd2c", PlatformComponents: components,
			State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING}}
}

func TestClusterComponentsTable(t *testing.T) {
	since := timestamppb.New(time.Date(2026, 11, 13, 5, 0, 0, 0, time.UTC))
	long := `update failed: failed to upgrade release: "kyverno" has no deployed releases ` + strings.Repeat("x", 200)
	tests := []struct {
		name       string
		components []*cloudv1.PlatformComponent
		want       []string
		wantNot    []string
	}{
		{name: "none", want: []string{"No platform components"}},
		{name: "all ready",
			components: []*cloudv1.PlatformComponent{{Name: "karpenter", Version: "1.15.1", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY, Since: since}},
			want:       []string{"Release shipped-f874cd2c", "NAME", "karpenter", "1.15.1", "Ready", "2026-11-13T05:00:00Z"},
			wantNot:    []string{"last error", "Helm state"}},
		{name: "one failed: the whole error follows the table",
			components: []*cloudv1.PlatformComponent{
				{Name: "karpenter", Version: "1.15.1", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY, Since: since},
				{Name: "kyverno", Version: "3.10.0", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Since: since,
					Message: "chart kyverno: " + long, Chart: "kyverno", HelmState: "uninstalling", LastError: long, Failures: 12, Recoveries: 1}},
			want: []string{"kyverno", "Failed", "...", "kyverno: chart kyverno, Helm state uninstalling, 12 failed attempts, recovered 1 times",
				"last error: " + long}},
		{name: "not in the release", components: []*cloudv1.PlatformComponent{{Name: "external-secrets", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED,
			Message: "not in release 2026-11-c"}}, want: []string{"external-secrets", "Failed", "not in release 2026-11-c"}, wantNot: []string{"last error"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			svc := &oneCluster{cluster: clusterWithComponents(tt.components...)}

			err := Run(t.Context(), ClusterComponents, Clients{Cluster: svc}, Options{Tenant: "acme", ID: kubeconfigCluster}, &out, OutputTable)

			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out.String())
				}
			}
			for _, not := range tt.wantNot {
				if strings.Contains(out.String(), not) {
					t.Errorf("output has %q:\n%s", not, out.String())
				}
			}
		})
	}
}

// As data, only the components are printed, with every field.
func TestClusterComponentsAsData(t *testing.T) {
	var out bytes.Buffer
	svc := &oneCluster{cluster: clusterWithComponents(&cloudv1.PlatformComponent{Name: "kyverno", Version: "3.10.0",
		Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Chart: "kyverno", HelmState: "uninstalling", LastError: "boom", Failures: 3})}

	err := Run(t.Context(), ClusterComponents, Clients{Cluster: svc}, Options{Tenant: "acme", ID: kubeconfigCluster}, &out, OutputJSON)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.Join(strings.Fields(out.String()), "")
	for _, want := range []string{`"platformRelease":"shipped-f874cd2c"`, `"helmState":"uninstalling"`, `"lastError":"boom"`, `"failures":3`} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %s:\n%s", want, out.String())
		}
	}
	if strings.Contains(got, "CLUSTER_STATE_RUNNING") {
		t.Errorf("the rest of the status is printed:\n%s", out.String())
	}
}

func TestClusterComponentsFailures(t *testing.T) {
	tests := []struct {
		name string
		svc  *oneCluster
		opts Options
		want string
	}{
		{"no tenant", &oneCluster{cluster: clusterWithComponents()}, Options{ID: kubeconfigCluster}, "--tenant"},
		{"the API fails", &oneCluster{err: errors.New("boom")}, Options{Tenant: "acme", ID: kubeconfigCluster}, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Run(t.Context(), ClusterComponents, Clients{Cluster: tt.svc}, tt.opts, &bytes.Buffer{}, OutputTable)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run = %v, want an error naming %q", err, tt.want)
			}
		})
	}
}
