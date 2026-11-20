package controller

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// shippedVars are the placeholder values of a running cluster with IRSA.
func shippedVars(name string) component.Vars {
	cluster := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}
	cluster.Spec.Region = "us-east-1"
	cluster.Spec.AWS = &cloudv1alpha1.AWSClusterSpec{IRSA: ptr.To(true)}
	cluster.Status.OIDCIssuerURL = "https://oidc.eks.us-east-1.amazonaws.com/id/ABC"
	cluster.Status.Endpoint = &cloudv1alpha1.ClusterEndpoint{URL: "https://abc.eks.amazonaws.com"}
	network := &cloudv1alpha1.Network{}
	network.Status.Resources = &cloudv1alpha1.NetworkResources{CloudNetworkID: "vpc-0abc"}
	account := &cloudv1alpha1.CloudAccount{Spec: cloudv1alpha1.CloudAccountSpec{AWS: &cloudv1alpha1.AWSAccount{AccountID: "444455556666"}}}
	return component.VarsFor(cluster, network, account, component.IAMOutputs{
		RoleARN: "arn:aws:iam::444455556666:role/prod-" + name + "-role", RoleName: "prod-" + name + "-role",
		PolicyARN: "arn:aws:iam::444455556666:policy/prod-" + name + "-policy", NodeRoleName: "prod-node-role",
		ClusterPrefix: "prod"})
}

// The shipped catalog and the shipped release plan on every Kubernetes
// minor with NaaS enabled: every Platform component is installed (none left out, none
// waiting on a running cluster), each chart at a pinned version, in an
// order that puts a component after what it requires, with valid names.
func TestShippedPlatformComponentsPlan(t *testing.T) {
	defs, _ := shippedCatalog(t)
	items := make([]cloudv1alpha1.ComponentDefinition, 0, len(defs))
	for _, d := range defs {
		items = append(items, *d)
	}
	entries, err := component.PlatformEntries(items)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aws-load-balancer-controller", "capsule", "cluster-autoscaler", "external-secrets", "istio-base", "istio-cni",
		"istio-ingress-gateway", "istiod", "karpenter", "kyverno", "ztunnel"} // kyverno ships enabled (d510cd1)
	for _, k := range DefaultRelease().Spec.Content.Kubernetes {
		t.Run(k.Minor, func(t *testing.T) {
			pins := map[string]component.Pin{}
			for _, c := range k.Components {
				values, err := component.PinValues(c.Values)
				if err != nil {
					t.Fatal(err)
				}
				pins[c.Name] = component.Pin{Version: c.Version, Values: values}
			}
			in := component.PlatformInput{Entries: entries, Release: DefaultReleaseName, Pins: pins,
				Cluster: component.Cluster{Name: "prod", Running: true, KubernetesMinor: k.Minor, IRSA: true},
				Vars:    shippedVars,
				Plan: crossplaneaws.PlatformPlan{Namespace: "tenant-acme", TenantID: "acme", ClusterID: "c-1",
					ClusterPrefix: "prod-1a2b3c4d", NodeGroupName: "prod-1a2b3c4d-default"},
				IAM: crossplaneaws.ComponentIAMPlan{Namespace: "tenant-acme", ProviderConfigName: "aws", TenantID: "acme",
					ClusterID: "c-1", ClusterName: "prod", ClusterPrefix: "prod-1a2b3c4d", Region: "us-east-1"},
			}

			plan, skipped := component.PlanPlatform(in)

			if len(skipped) != 0 {
				t.Fatalf("left out: %v", skipped)
			}
			var got []string
			position := map[string]int{}
			for i, c := range plan.Components {
				got = append(got, c.Name)
				position[c.Name] = i
				if c.WaitsFor != "" {
					t.Errorf("%s waits for %s", c.Name, c.WaitsFor)
				}
				for _, r := range c.Requires {
					if before, ok := position[r]; !ok || before >= i {
						t.Errorf("%s is planned before %s, which it requires", c.Name, r)
					}
				}
				for _, e := range entries {
					if e.Name == c.Name && len(c.Companions) != len(e.Install.Companions) {
						t.Errorf("%s: %d of %d companions are pinned", c.Name, len(c.Companions), len(e.Install.Companions))
					}
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("planned %v, want %v", got, want)
			}
			for _, item := range crossplaneaws.PlatformSet(plan) {
				if errs := validation.IsDNS1123Subdomain(item.Object.GetName()); len(errs) > 0 {
					t.Errorf("%T %q: %v", item.Object, item.Object.GetName(), errs)
				}
				u, ok := item.Object.(*unstructured.Unstructured)
				if !ok {
					continue
				}
				if v, _, _ := unstructured.NestedString(u.Object, "spec", "forProvider", "chart", "version"); v == "" {
					t.Errorf("Release %s has no chart version", u.GetName())
				}
			}
		})
	}
}
