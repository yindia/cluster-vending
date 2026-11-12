package convert_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/deprecation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestDeprecationsRoundTrip(t *testing.T) {
	at := timestamppb.New(time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC))
	in := &cloudv1.ClusterDeprecations{TargetVersion: "1.37", ScannedAt: at, AttemptedAt: at, Total: 2, Removed: 1, Truncated: true,
		Error: "unreachable", Findings: []*cloudv1.DeprecationFinding{
			convert.DeprecationFinding(deprecation.Finding{Namespace: "web", Kind: "Ingress", Name: "site", APIVersion: "extensions/v1beta1",
				Component: "k8s", DeprecatedIn: "v1.14.0", RemovedIn: "v1.22.0", ReplacementAPI: "networking.k8s.io/v1",
				ReplacementAvailableIn: "v1.19.0", Source: deprecation.SourceManifest, Removed: true}),
			convert.DeprecationFinding(deprecation.Finding{Kind: "CronJob", Name: "nightly", APIVersion: "batch/v1beta1",
				Source: deprecation.SourceHelmRelease, HelmRelease: "jobs/nightly"}),
		}}

	kind := convert.DeprecationsToAPI(in)
	out := convert.DeprecationsFromAPI(kind)

	if !proto.Equal(in, out) {
		t.Fatalf("round trip:\n got %v\nwant %v", out, in)
	}
	if kind.Findings[0].Source != cloudv1alpha1.DeprecationSourceManifest || kind.Findings[1].Source != cloudv1alpha1.DeprecationSourceHelmRelease {
		t.Errorf("sources = %s, %s", kind.Findings[0].Source, kind.Findings[1].Source)
	}
	if convert.DeprecationsToAPI(nil) != nil || convert.DeprecationsFromAPI(nil) != nil {
		t.Error("no scan must stay no scan")
	}
}

func TestDeprecationsToAPIKeepsWhatTheKindHolds(t *testing.T) {
	in := &cloudv1.ClusterDeprecations{Error: strings.Repeat("x", 5000)}
	for range cloudv1alpha1.MaxDeprecationFindings + 5 {
		in.Findings = append(in.Findings, &cloudv1.DeprecationFinding{Kind: "Ingress"})
	}

	kind := convert.DeprecationsToAPI(in)

	if len(kind.Findings) != cloudv1alpha1.MaxDeprecationFindings || !kind.Truncated || len(kind.Error) != 1024 {
		t.Fatalf("%d findings, truncated %v, error of %d bytes", len(kind.Findings), kind.Truncated, len(kind.Error))
	}
}

func TestDeprecationCondition(t *testing.T) {
	tests := []struct {
		name       string
		in         cloudv1alpha1.ClusterDeprecations
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string
	}{
		{"nothing found", cloudv1alpha1.ClusterDeprecations{TargetVersion: "1.37"}, metav1.ConditionTrue, "Scanned",
			"no deprecated API versions in use for Kubernetes 1.37"},
		{"deprecated only", cloudv1alpha1.ClusterDeprecations{TargetVersion: "1.37", Total: 1}, metav1.ConditionTrue, "DeprecatedAPIsInUse",
			"1 object uses a deprecated API version"},
		{"removed", cloudv1alpha1.ClusterDeprecations{TargetVersion: "1.37", Total: 5, Removed: 3}, metav1.ConditionTrue, "RemovedAPIsInUse",
			"3 objects use an API version removed in Kubernetes 1.37"},
		{"the scan failed", cloudv1alpha1.ClusterDeprecations{TargetVersion: "1.37", Total: 5, Removed: 3, Error: "unreachable"},
			metav1.ConditionFalse, "ScanFailed", "unreachable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convert.DeprecationCondition(&tt.in, 7)
			if got.Type != cloudv1alpha1.ConditionDeprecationScan || got.Status != tt.wantStatus || got.Reason != tt.wantReason ||
				got.Message != tt.wantMsg || got.ObservedGeneration != 7 {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
