package component

import (
	"testing"

	"connectrpc.com/connect/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// withGCPScanner is the world with scanner-gcp, the tenant component's
// definition for GCP clusters; the world's cluster runs on AWS.
func withGCPScanner(t *testing.T) world {
	t.Helper()
	w := newWorld(t)
	w.catalog.defs = append(w.catalog.defs, cloudv1alpha1.ComponentDefinition{ObjectMeta: metav1.ObjectMeta{Name: "scanner-gcp"},
		Spec: cloudv1alpha1.ComponentDefinitionSpec{Description: "The scanner for GCP.", Namespace: "scanner",
			Provider: cloudv1alpha1.CloudProviderGCP, Install: chart("scanner")}})
	return w
}

func providers(defs []*cloudv1.ComponentDefinition) map[string]cloudv1.CloudProvider {
	out := map[string]cloudv1.CloudProvider{}
	for _, d := range defs {
		out[d.GetName()] = d.GetProvider()
	}
	return out
}

func TestCatalogNamesEachDefinitionsCloud(t *testing.T) {
	w := withGCPScanner(t)

	all, err := w.svc.ListComponentDefinitions(t.Context(), &cloudv1.ListComponentDefinitionsRequest{TenantId: tenant})
	if err != nil {
		t.Fatal(err)
	}
	onCluster, err := w.svc.ListComponentDefinitions(t.Context(), &cloudv1.ListComponentDefinitionsRequest{TenantId: tenant, ClusterId: w.cluster.ID})
	if err != nil {
		t.Fatal(err)
	}

	got := providers(all.GetDefinitions())
	if got["scanner"] != cloudv1.CloudProvider_CLOUD_PROVIDER_AWS || got["scanner-gcp"] != cloudv1.CloudProvider_CLOUD_PROVIDER_GCP {
		t.Errorf("providers %v", got)
	}
	if _, ok := providers(onCluster.GetDefinitions())["scanner-gcp"]; ok {
		t.Error("an AWS cluster's catalog lists a definition for GCP")
	}
	if _, ok := providers(onCluster.GetDefinitions())["scanner"]; !ok {
		t.Error("an AWS cluster's catalog does not list a definition for AWS")
	}
}

func TestEnableIsRefusedOnAnotherCloud(t *testing.T) {
	w := withGCPScanner(t)

	_, err := w.svc.EnableComponent(t.Context(), &cloudv1.EnableComponentRequest{TenantId: tenant,
		Component: &cloudv1.Component{ClusterId: w.cluster.ID, Component: "scanner-gcp"}})

	requireCode(t, err, connect.CodeFailedPrecondition, "scanner-gcp is for gcp", "runs on aws")
}

func TestListOnAClusterLeavesOutOtherClouds(t *testing.T) {
	w := withGCPScanner(t)

	res, err := w.svc.ListComponents(t.Context(), &cloudv1.ListComponentsRequest{TenantId: tenant, ClusterId: w.cluster.ID})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range res.GetComponents() {
		if c.GetComponent() == "scanner-gcp" {
			t.Fatalf("listed %+v on an AWS cluster", c)
		}
	}
}
