package aws_test

import (
	"encoding/json"
	"strings"
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

func chart(name, version string) crossplaneaws.HelmChart {
	return crossplaneaws.HelmChart{Repository: "https://charts.example.com", Name: name, Version: version}
}

// platformPlan is three components: base (a CRD chart and a companion),
// mesh requiring base, and edge requiring mesh, with an IAM role.
func platformPlan() crossplaneaws.PlatformPlan {
	c := clusterPlanForNames()
	iam := componentPlan(crossplaneaws.ComponentAccessPodIdentity)
	iam.Component = "edge"
	return crossplaneaws.PlatformPlan{
		Namespace: c.Namespace, TenantID: c.TenantID, ClusterID: c.ClusterID, ClusterPrefix: c.Prefix,
		NodeGroupName: c.Prefix + "-default",
		Components: []crossplaneaws.PlatformComponentPlan{
			{Name: "base", Namespace: "base-system",
				CRDs:       &crossplaneaws.HelmReleasePlan{Name: crossplaneaws.CRDsReleaseName("base"), Chart: chart("base-crd", "1.2.0")},
				Main:       crossplaneaws.HelmReleasePlan{Name: "base", Chart: chart("base", "1.2.0"), SkipCRDs: true, Values: map[string]any{"replicas": int64(2)}},
				Companions: []crossplaneaws.HelmReleasePlan{{Name: "base-extra", Chart: chart("base-extra", "0.1.0"), Values: map[string]any{"enabled": true}}}},
			{Name: "mesh", Namespace: "mesh-system", Requires: []string{"base"},
				Main: crossplaneaws.HelmReleasePlan{Name: "mesh", Chart: chart("mesh", "2.0.0")}},
			{Name: "edge", Namespace: "edge", Requires: []string{"mesh"}, IAM: &iam,
				Main: crossplaneaws.HelmReleasePlan{Name: "edge", Chart: chart("edge", "3.0.0")}},
		},
	}
}

func releasesOf(set sequence.Set) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, item := range set {
		if u, ok := item.Object.(*unstructured.Unstructured); ok && u.GroupVersionKind() == crossplaneaws.HelmReleaseGVK {
			out = append(out, u)
		}
	}
	return out
}

// A Release is exactly the fields of provider-helm v1.5.0's
// releases.helm.m.crossplane.io v1beta1 CRD the platform uses: the Helm
// release name as the external name, the cluster's ProviderConfig, a large
// rollbackLimit, the chart, the namespace it installs into, its values
// and skipCRDs.
func TestHelmReleaseFields(t *testing.T) {
	plan := platformPlan()

	releases := releasesOf(crossplaneaws.PlatformSet(plan))

	if len(releases) != 5 {
		t.Fatalf("%d releases, want 5", len(releases))
	}
	got, err := json.Marshal(releases[1].Object)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"apiVersion":"helm.m.crossplane.io/v1beta1","kind":"Release","metadata":{"annotations":{"crossplane.io/external-name":"base"},"labels":{"app.kubernetes.io/managed-by":"cluster-vending","cluster-vending.io/cluster":"c-1","cluster-vending.io/component":"base","cluster-vending.io/tenant-id":"acme"},"name":"` +
		plan.ClusterPrefix + `-base","namespace":"tenant-acme"},"spec":{"forProvider":{"chart":{"name":"base","repository":"https://charts.example.com","version":"1.2.0"},"namespace":"base-system","skipCRDs":true,"values":{"replicas":2}},"providerConfigRef":{"kind":"ProviderConfig","name":"` +
		plan.ClusterPrefix + `-helm"},"rollbackLimit":100}}`
	if string(got) != want {
		t.Errorf("Release\n got %s\nwant %s", got, want)
	}
	crds := releases[0]
	if crds.GetAnnotations()["crossplane.io/external-name"] != "base-crds" {
		t.Errorf("CRD chart release name %q", crds.GetAnnotations()["crossplane.io/external-name"])
	}
	if _, found, _ := unstructured.NestedMap(crds.Object, "spec", "forProvider", "values"); found {
		t.Error("the CRD chart gets no values")
	}
	if skip, _, _ := unstructured.NestedBool(crds.Object, "spec", "forProvider", "skipCRDs"); skip {
		t.Error("the CRD chart itself must install its CRDs")
	}
}

// Charts install in order: the CRD chart, the chart, its companions; a
// component after the last chart of what it requires; a component with a
// role after its policies are attached. Every chart waits for the
// kubeconfig and the node group.
func TestPlatformSetOrder(t *testing.T) {
	plan := platformPlan()

	set := crossplaneaws.PlatformSet(plan)

	var names []string
	after := map[string]int{}
	for _, item := range set {
		if u, ok := item.Object.(*unstructured.Unstructured); ok {
			names = append(names, strings.TrimPrefix(u.GetName(), plan.ClusterPrefix+"-"))
			after[names[len(names)-1]] = len(item.After)
		}
	}
	if got, want := strings.Join(names, ","), "base-crds,base,base-extra,mesh,edge"; got != want {
		t.Fatalf("release order %s, want %s", got, want)
	}
	// ProviderConfig + node group = 2; "installed" adds Ready and deployed.
	for name, want := range map[string]int{
		"base-crds": 2, "base": 4, "base-extra": 4, "mesh": 4, // mesh: after base-extra
		"edge": 2 + 2 + 2, // after mesh, and its two attachments
	} {
		if after[name] != want {
			t.Errorf("%s waits for %d conditions, want %d", name, after[name], want)
		}
	}
}

// Deletion is the reverse, as Usages: a chart keeps the chart it was
// installed after, a component the last chart of what it requires, and a
// component's first chart its role's attachments.
func TestPlatformSetUsages(t *testing.T) {
	plan := platformPlan()

	set := crossplaneaws.PlatformSet(plan)

	pairs := map[string]bool{}
	for _, item := range set {
		u, ok := item.Object.(*protectionv1beta1.Usage)
		if !ok || u.Spec.By.Kind != "Release" {
			continue
		}
		short := func(name string) string { return strings.TrimPrefix(name, plan.ClusterPrefix+"-") }
		pairs[short(u.Spec.Of.ResourceRef.Name)+"<-"+short(u.Spec.By.ResourceRef.Name)] = true
		if len(item.After) == 0 {
			t.Errorf("Usage %s waits for nothing: it must wait with its user", u.Name)
		}
	}
	for _, want := range []string{"base-crds<-base", "base<-base-extra", "base-extra<-mesh", "mesh<-edge",
		"edge-attach-policy<-edge", "edge-attach-1<-edge"} {
		if !pairs[want] {
			t.Errorf("no Usage %s; have %v", want, pairs)
		}
	}
	if len(pairs) != 6 {
		t.Errorf("%d Usages by a Release, want 6: %v", len(pairs), pairs)
	}
}

// A component whose values wait keeps its charts (and their Usages) back
// and says what for; its role is still applied.
func TestPlatformSetHoldsBackAComponentThatWaits(t *testing.T) {
	plan := platformPlan()
	plan.Components[2].WaitsFor = "${iam.roleArn}"

	set := crossplaneaws.PlatformSet(plan)

	held, roles := 0, 0
	for _, item := range set {
		name := item.Object.GetName()
		switch {
		case item.Build == nil:
			if strings.HasSuffix(name, "-edge-role") {
				roles++
			}
		case strings.Contains(name, "edge"):
			obj, reason, err := item.Build(t.Context(), nil)
			if obj != nil || err != nil || reason != "${iam.roleArn}" {
				t.Errorf("%s: Build = %v, %q, %v", name, obj, reason, err)
			}
			held++
		default:
			t.Errorf("%s is held back, but only edge waits", name)
		}
	}
	if held != 4 { // the Release and the three Usages it is the user of
		t.Errorf("%d objects held back, want 4", held)
	}
	if roles != 1 {
		t.Errorf("the role must still be applied; found %d", roles)
	}
}

// Without a node group name nothing waits for one.
func TestPlatformSetWithoutANodeGroup(t *testing.T) {
	plan := platformPlan()
	plan.NodeGroupName = ""

	set := crossplaneaws.PlatformSet(plan)

	if got := len(set[0].After); got != 1 {
		t.Errorf("the first chart waits for %d conditions, want only the ProviderConfig", got)
	}
}

// Every object name is a valid object name, also for long component
// names, and stays unique.
func TestPlatformNamesAreValid(t *testing.T) {
	plan := platformPlan()
	long := strings.Repeat("very-long-component-name-", 3) + "x"
	plan.Components = append(plan.Components, crossplaneaws.PlatformComponentPlan{Name: long, Namespace: "x",
		CRDs: &crossplaneaws.HelmReleasePlan{Name: crossplaneaws.CRDsReleaseName(long), Chart: chart("x-crd", "1")},
		Main: crossplaneaws.HelmReleasePlan{Name: long, Chart: chart("x", "1")}})

	set := crossplaneaws.PlatformSet(plan)

	seen := map[string]bool{}
	for _, item := range set {
		name := item.Object.GetName()
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			t.Errorf("%T %q: %v", item.Object, name, errs)
		}
		key := item.Object.GetObjectKind().GroupVersionKind().Kind + "/" + name
		if seen[key] {
			t.Errorf("%s rendered twice", key)
		}
		seen[key] = true
	}
	if a, b := crossplaneaws.HelmReleaseObjectName(plan.ClusterPrefix, long), crossplaneaws.HelmReleaseObjectName(plan.ClusterPrefix, long+"-crds"); a == b || len(a) > 63 {
		t.Errorf("long names: %q and %q", a, b)
	}
	names := plan.ReleaseObjectNames(plan.Components[0])
	if len(names) != 3 || names[0] != plan.ClusterPrefix+"-base-crds" || names[2] != plan.ClusterPrefix+"-base-extra" {
		t.Errorf("ReleaseObjectNames = %v", names)
	}
}

// Test registries: the chart source's flags reach the Release.
func TestHelmReleaseRegistryFlags(t *testing.T) {
	plan := platformPlan()
	plan.Components = plan.Components[1:2]
	plan.Components[0].Requires = nil
	plan.Components[0].Main.Chart.InsecureSkipTLSVerify = true
	plan.Components[0].Main.Chart.PlainHTTP = true

	releases := releasesOf(crossplaneaws.PlatformSet(plan))

	for _, field := range []string{"insecureSkipTLSVerify", "plainHTTP"} {
		if v, _, _ := unstructured.NestedBool(releases[0].Object, "spec", "forProvider", field); !v {
			t.Errorf("%s is not set", field)
		}
	}
}
