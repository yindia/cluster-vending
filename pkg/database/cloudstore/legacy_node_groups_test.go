package cloudstore_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A Cluster object written before the platform owned node groups still
// carries spec.defaultNodePool, spec.nodeGroups and spec.aws.defaultNodePool.
// The row read from it has none of them (the kind has no such fields), and
// writing the row back does not bring them back.
func TestLegacyNodeGroupFieldsOnTheObjectAreIgnored(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	before, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(cloudv1alpha1.GroupVersion.WithKind("Cluster"))
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "prod"}, obj); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"defaultNodePool": map[string]any{"instanceTypes": []any{"m6i.large"}, "scaling": map[string]any{"minSize": int64(1), "maxSize": int64(3)}},
		"nodeGroups":      []any{map[string]any{"name": "batch", "instanceFamilies": []any{"c7g"}}},
	}
	for k, v := range legacy {
		if err := unstructured.SetNestedField(obj.Object, v, "spec", k); err != nil {
			t.Fatal(err)
		}
	}
	if err := unstructured.SetNestedField(obj.Object, map[string]any{"amiType": "CUSTOM",
		"launchTemplate": map[string]any{"id": "lt-0123456789abcdef0", "version": "3"}}, "spec", "aws", "defaultNodePool"); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	row, err := w.s.Clusters().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"defaultNodePool", "nodeGroups", "launchTemplate"} {
		if strings.Contains(string(row.Spec), field) {
			t.Fatalf("the row's spec carries %s: %s", field, row.Spec)
		}
	}
	got, want := &cloudv1.ClusterSpec{}, &cloudv1.ClusterSpec{}
	if err := protojson.Unmarshal(row.Spec, got); err != nil {
		t.Fatal(err)
	}
	if err := protojson.Unmarshal(before.Spec, want); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, want) {
		t.Fatalf("spec with legacy fields on the object = %v, want %v", got, want)
	}
	row.DisplayName = "again"
	if err := w.s.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	typed := &cloudv1alpha1.Cluster{}
	if err := w.k.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "prod"}, typed); err != nil {
		t.Fatal(err)
	}
	if typed.Spec.DisplayName != "again" {
		t.Fatalf("the write of the row did not land: %+v", typed.Spec)
	}
}
