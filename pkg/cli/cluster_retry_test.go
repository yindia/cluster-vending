package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

type retryService struct {
	cloudv1connect.ClusterServiceClient
	got *cloudv1.RetryClusterRequest
	res *cloudv1.RetryClusterResponse
}

func (s *retryService) RetryCluster(_ context.Context, req *cloudv1.RetryClusterRequest) (*cloudv1.RetryClusterResponse, error) {
	s.got = req
	return s.res, nil
}

const retryClusterID = "01a1dbc3-66de-7e9e-83b7-5e4573748830"

// cluster retry sends what it was given and prints one row per item.
func TestClusterRetry(t *testing.T) {
	api := &retryService{res: &cloudv1.RetryClusterResponse{Items: []*cloudv1.RetryItem{
		{Name: "kyverno", Kind: cloudv1.RetryItemKind_RETRY_ITEM_KIND_PLATFORM_COMPONENT, Action: cloudv1.RetryAction_RETRY_ACTION_RETRY,
			Message: "chart kyverno is tried again on the next pass"},
		{Name: "karpenter", Kind: cloudv1.RetryItemKind_RETRY_ITEM_KIND_PLATFORM_COMPONENT, Action: cloudv1.RetryAction_RETRY_ACTION_FIX,
			Message: "change this first; it installs by itself once done: set spec.aws.irsa to true"},
		{Name: "upgrade", Kind: cloudv1.RetryItemKind_RETRY_ITEM_KIND_UPGRADE, Action: cloudv1.RetryAction_RETRY_ACTION_RETRY,
			Message: "the automatic upgrade to 1.37 is in progress again"},
	}}}
	var out bytes.Buffer

	err := Run(t.Context(), ClusterRetry, Clients{Cluster: api},
		Options{Tenant: "acme", ID: retryClusterID, RetryComponents: []string{"kyverno", "karpenter"}, RetryUpgrade: true}, &out, OutputTable)

	if err != nil {
		t.Fatal(err)
	}
	if api.got.GetClusterId() != retryClusterID || !slices.Equal(api.got.GetComponents(), []string{"kyverno", "karpenter"}) || !api.got.GetUpgrade() {
		t.Errorf("request = %v", api.got)
	}
	got := out.String()
	for _, want := range []string{"NAME", "KIND", "ACTION", "kyverno", "PlatformComponent", "Retry", "karpenter", "Fix", "spec.aws.irsa", "upgrade", "Upgrade"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

// Nothing failed: it says so instead of an empty table.
func TestClusterRetryNothingFailed(t *testing.T) {
	var out bytes.Buffer

	err := Run(t.Context(), ClusterRetry, Clients{Cluster: &retryService{res: &cloudv1.RetryClusterResponse{}}},
		Options{Tenant: "acme", ID: retryClusterID}, &out, OutputTable)

	if err != nil || !strings.Contains(out.String(), "nothing has failed") {
		t.Fatalf("%v:\n%s", err, out.String())
	}
}
