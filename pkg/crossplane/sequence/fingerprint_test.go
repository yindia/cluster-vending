package sequence_test

import (
	"context"
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func vpc(name, cidr string) *awsec2.VPC {
	v := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	v.Spec.ForProvider.CidrBlock = &cidr
	return v
}

// The same render has the same fingerprints; a changed body, an object
// added and one removed are each seen; a Build item counts by identity
// and an Adjust is part of the body.
func TestFingerprints(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	build := sequence.Item{Object: vpc("built", ""), Build: func(context.Context, sequence.Observed) (client.Object, string, error) {
		return nil, "", nil
	}}
	before := sequence.Set{{Object: vpc("a", "10.0.0.0/16")}, {Object: vpc("b", "10.1.0.0/16")}, build}

	applied, err := sequence.Fingerprints(before, scheme)
	if err != nil {
		t.Fatal(err)
	}
	again, err := sequence.Fingerprints(sequence.Set{{Object: vpc("a", "10.0.0.0/16")}, {Object: vpc("b", "10.1.0.0/16")}, build}, scheme)
	if err != nil {
		t.Fatal(err)
	}
	if d := sequence.Diff(applied, again); d.Total() != 0 || sequence.Digest(applied) != sequence.Digest(again) {
		t.Fatalf("the same render differs: %+v", d)
	}

	adjusted := sequence.Item{Object: vpc("a", "10.0.0.0/16"), Adjust: func(u *unstructured.Unstructured) { u.SetLabels(map[string]string{"x": "y"}) }}
	now, err := sequence.Fingerprints(sequence.Set{adjusted, {Object: vpc("c", "10.2.0.0/16")}, build}, scheme)
	if err != nil {
		t.Fatal(err)
	}

	if sequence.Digest(applied) == sequence.Digest(now) {
		t.Error("another render has the same digest")
	}
	d := sequence.Diff(applied, now)
	if d != (sequence.Changes{Changed: 1, Added: 1, Removed: 1}) {
		t.Errorf("diff = %+v, want one each", d)
	}
	if got := d.String(); got != "3 objects change (1 added, 1 removed)" {
		t.Errorf("String = %q", got)
	}
	if got := (sequence.Changes{Changed: 1}).String(); got != "1 object changes" {
		t.Errorf("String = %q", got)
	}
}
