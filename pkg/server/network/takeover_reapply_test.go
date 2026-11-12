package network

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A takeover used to read each imported object and edit it in place; it
// now applies the imported set again, rendered as owned. These are the
// edits the old code made, kept here as the reference the new result is
// compared with, object by object.

// legacyTaggable are the kinds the in-place retag set label tags on.
var legacyTaggable = map[string]bool{
	"VPC": true, "InternetGateway": true, "Subnet": true, "EIP": true, "NATGateway": true, "RouteTable": true,
	"FlowLog": true, "Group": true, "Role": true, "NetworkACL": true, "DefaultNetworkACL": true,
}

// legacyOwn is what the in-place takeover did to one object: full
// management (without Delete for a retained one) and the load balancer
// role tag on subnets.
func legacyOwn(t *testing.T, u *unstructured.Unstructured) {
	t.Helper()
	policies := []string{"*"}
	if u.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true" {
		policies = policies[:0]
		for _, action := range crossplaneaws.PolicyNoDelete {
			policies = append(policies, string(action))
		}
	}
	if err := unstructured.SetNestedStringSlice(u.Object, policies, "spec", "managementPolicies"); err != nil {
		t.Fatal(err)
	}
	if u.GetKind() != "Subnet" {
		return
	}
	tag := crossplaneaws.TagInternalELB
	if u.GetLabels()[crossplane.LabelSubnetRole] == string(crossplaneaws.SubnetPublic) {
		tag = crossplaneaws.TagELB
	}
	if err := unstructured.SetNestedField(u.Object, "1", "spec", "forProvider", "tags", tag); err != nil {
		t.Fatal(err)
	}
}

// legacyRetag is what the in-place retag did to one object: the labels as
// tags, the tags that were labels before and are not any more removed.
func legacyRetag(u *unstructured.Unstructured, labels map[string]string) {
	if !legacyTaggable[u.GetKind()] || u.GetAnnotations()[crossplaneaws.RetainAnnotation] == "true" {
		return
	}
	tags, _, _ := unstructured.NestedStringMap(u.Object, "spec", "forProvider", "tags")
	if tags == nil {
		tags = map[string]string{}
	}
	annotations := u.GetAnnotations()
	for _, k := range strings.Split(annotations[crossplaneaws.LabelTagsAnnotation], ",") {
		if _, kept := labels[k]; !kept && k != "" {
			delete(tags, k)
		}
	}
	keys := make([]string, 0, len(labels))
	for k, v := range labels {
		keys = append(keys, k)
		tags[k] = v
	}
	slices.Sort(keys)
	if want := strings.Join(keys, ","); annotations[crossplaneaws.LabelTagsAnnotation] != want {
		annotations[crossplaneaws.LabelTagsAnnotation] = want
	}
	u.SetAnnotations(annotations)
	_ = unstructured.SetNestedStringMap(u.Object, tags, "spec", "forProvider", "tags")
}

// liveObjects are the network's objects in the management cluster, by
// kind and name.
func (f fixture) liveObjects(t *testing.T, id string) map[string]*unstructured.Unstructured {
	t.Helper()
	out := map[string]*unstructured.Unstructured{}
	for _, list := range crossplaneaws.NetworkLists() {
		if err := f.kube.List(t.Context(), list, client.MatchingLabels{crossplane.LabelNetwork: id}); err != nil {
			t.Fatal(err)
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			obj := item.(client.Object)
			gvk, err := apiutil.GVKForObject(obj, f.kube.Scheme())
			if err != nil {
				t.Fatal(err)
			}
			content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
			if err != nil {
				t.Fatal(err)
			}
			u := &unstructured.Unstructured{Object: content}
			u.SetGroupVersionKind(gvk)
			out[gvk.Kind+"/"+obj.GetName()] = u
		}
	}
	return out
}

// desired is what of an object the platform decides: its spec, labels and
// annotations. An empty label-tags annotation is no annotation and an
// empty tags map is no tags: the in-place retag left both behind when the
// last label went.
func desired(t *testing.T, u *unstructured.Unstructured) string {
	t.Helper()
	u = u.DeepCopy()
	if tags, found, _ := unstructured.NestedMap(u.Object, "spec", "forProvider", "tags"); found && len(tags) == 0 {
		unstructured.RemoveNestedField(u.Object, "spec", "forProvider", "tags")
	}
	annotations := map[string]string{}
	for k, v := range u.GetAnnotations() {
		if k != crossplaneaws.LabelTagsAnnotation || v != "" {
			annotations[k] = v
		}
	}
	raw, err := json.Marshal(map[string]any{"spec": u.Object["spec"], "labels": u.GetLabels(), "annotations": annotations})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// sameAsInPlace compares the live objects with what the in-place edits
// would have left (want).
func (f fixture) sameAsInPlace(t *testing.T, step, id string, want map[string]*unstructured.Unstructured) {
	t.Helper()
	got := f.liveObjects(t, id)
	if len(got) != len(want) {
		t.Errorf("%s: %d objects, the in-place takeover had %d", step, len(got), len(want))
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s: %s is gone", step, name)
			continue
		}
		if have, wanted := desired(t, g), desired(t, w); have != wanted {
			t.Errorf("%s: %s differs from the in-place result\n got  %s\n want %s", step, name, have, wanted)
		}
	}
}

// everyImportedKind is a VPC with an object of every kind the importer
// renders: flow logs with a shared log group (owned without Delete) and a
// role, the default network ACL and a custom one with rules.
func everyImportedKind() *awscloud.Network {
	d := vpcWithFlowLogs()
	allow := awscloud.NetworkACLRule{Number: 100, Protocol: "-1", Action: "allow", CIDRBlock: "0.0.0.0/0"}
	allowOut := allow
	allowOut.Egress = true
	d.NetworkACLs = []awscloud.NetworkACL{
		{ID: "acl-default", Default: true, SubnetIDs: []string{"subnet-pa", "subnet-pb"}, Rules: []awscloud.NetworkACLRule{allow, allowOut}},
		{ID: "acl-private", SubnetIDs: []string{"subnet-xa", "subnet-xb"}, Rules: []awscloud.NetworkACLRule{allow, allowOut}},
	}
	return d
}

func (f fixture) setLabels(t *testing.T, id string, labels map[string]string) {
	t.Helper()
	if _, err := f.svc.UpdateNetwork(t.Context(), &cloudv1.UpdateNetworkRequest{TenantId: tenant,
		Network: &cloudv1.Network{Id: id, Labels: labels}, UpdateMask: mask("labels")}); err != nil {
		t.Fatalf("UpdateNetwork: %v", err)
	}
	f.reconcile(t, id)
}

// A takeover and the label updates after it leave every object, of every
// kind the importer renders, as the in-place edits did: the imported
// object with other management policies, the load balancer role tag on
// subnets, and the labels as tags. Nothing else of an object changes.
func TestTakeoverReappliesWhatTheInPlaceEditMade(t *testing.T) {
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: everyImportedKind()}
	f.svc.deps.AWS.StaticKeys = testKeys
	n := f.imported(t)
	id := n.GetId()

	want := f.liveObjects(t, id)
	kinds := map[string]bool{}
	for _, u := range want {
		kinds[u.GetKind()] = true
		if policies, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "managementPolicies"); !slices.Equal(policies, []string{"Observe"}) {
			t.Fatalf("%s %s is imported with policies %v", u.GetKind(), u.GetName(), policies)
		}
	}
	for _, kind := range []string{"VPC", "InternetGateway", "Subnet", "EIP", "NATGateway", "RouteTable", "Route",
		"RouteTableAssociation", "FlowLog", "Group", "Role", "DefaultNetworkACL", "NetworkACL", "NetworkACLRule"} {
		if !kinds[kind] {
			t.Fatalf("the import has no %s: the comparison would not cover it", kind)
		}
	}

	if _, err := f.svc.TakeOverNetwork(t.Context(), &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: id,
		AcknowledgeNotImported: true}); err != nil {
		t.Fatalf("TakeOverNetwork: %v", err)
	}
	f.reconcile(t, id)
	for _, u := range want {
		legacyOwn(t, u)
	}
	f.sameAsInPlace(t, "takeover", id, want)

	// Applied again, nothing changes.
	f.reconcile(t, id)
	f.sameAsInPlace(t, "second pass", id, want)

	for _, labels := range []map[string]string{
		{"team": "net", "env": "prod"}, // labels become tags
		{"team": "platform"},           // one changes, one goes
		{},                             // all go
	} {
		f.setLabels(t, id, labels)
		for _, u := range want {
			legacyRetag(u, labels)
		}
		f.sameAsInPlace(t, "labels "+strings.Join(keysOf(labels), ","), id, want)
	}
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// The one place the re-apply differs from the in-place edit, on purpose:
// a label with the key of a tag the import had replaces that tag, and
// when the label goes the import's tag is back. The in-place edit removed
// the tag, so a tag that came with the import was lost.
func TestALabelThatGoesGivesTheImportedTagBack(t *testing.T) {
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()} // its VPC is tagged Name=tf
	f.svc.deps.AWS.StaticKeys = testKeys
	n := f.takenOver(t)
	nameTag := func() string {
		t.Helper()
		vpc := f.liveObjects(t, n.GetId())["VPC/"+key(n, "vpc").Name]
		tags, _, _ := unstructured.NestedStringMap(vpc.Object, "spec", "forProvider", "tags")
		return tags["Name"]
	}
	f.setLabels(t, n.GetId(), map[string]string{"Name": "ours"})
	if got := nameTag(); got != "ours" {
		t.Fatalf("Name tag with the label = %q, want the label's value", got)
	}
	f.setLabels(t, n.GetId(), map[string]string{})
	if got := nameTag(); got != "tf" {
		t.Errorf("Name tag after the label went = %q, want the tag the import had", got)
	}
}
