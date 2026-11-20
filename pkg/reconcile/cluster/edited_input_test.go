package cluster

import (
	"maps"
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func addonsOf(names ...string) []*cloudv1.AwsAddon {
	var out []*cloudv1.AwsAddon
	for i := 0; i+1 < len(names); i += 2 {
		out = append(out, &cloudv1.AwsAddon{Name: names[i], Version: names[i+1]})
	}
	return out
}

func clusterWith(t *testing.T, addons []*cloudv1.AwsAddon) *cloudv1.Cluster {
	t.Helper()
	return &cloudv1.Cluster{Spec: &cloudv1.ClusterSpec{Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{Addons: addons}}}}
}

func TestEditedInputPins(t *testing.T) {
	tests := map[string]struct {
		input     []*cloudv1.AwsAddon // as sent
		edited    []*cloudv1.AwsAddon // the object's spec after the edit
		installed map[string]string
		want      map[string]string
	}{
		"new pin is kept": {
			edited: addonsOf("coredns", "v2", "kube-proxy", "v1"), installed: map[string]string{"coredns": "v1", "kube-proxy": "v1"},
			want: map[string]string{"coredns": "v2"}},
		"resolved versions are no pins": {
			edited: addonsOf("coredns", "v1"), installed: map[string]string{"coredns": "v1"}, want: map[string]string{}},
		"an old pin stays while it is the edit's": {
			input: addonsOf("coredns", "v1"), edited: addonsOf("coredns", "v1"), installed: map[string]string{"coredns": "v1"},
			want: map[string]string{"coredns": "v1"}},
		"a removed pin is gone": {
			input: addonsOf("coredns", "v1"), edited: addonsOf("coredns", ""), installed: map[string]string{"coredns": "v1"},
			want: map[string]string{}},
		"a new add-on with a version is pinned": {
			edited: addonsOf("aws-ebs-csi-driver", "v3"), want: map[string]string{"aws-ebs-csi-driver": "v3"}},
		"a removed add-on is no pin": {
			input: addonsOf("coredns", "v1"), want: map[string]string{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in, err := Encode(clusterWith(t, tt.input))
			if err != nil {
				t.Fatal(err)
			}
			spec, err := Encode(clusterWith(t, tt.edited).GetSpec())
			if err != nil {
				t.Fatal(err)
			}
			row := &model.Cluster{Input: in}
			row.ID, row.Spec = "c1", spec
			got, err := EditedInput(row, tt.installed)
			if err != nil {
				t.Fatal(err)
			}
			out := &cloudv1.Cluster{}
			if err := Lenient.Unmarshal(got, out); err != nil {
				t.Fatal(err)
			}
			if pins := PinnedAddons(out.GetSpec()); !maps.Equal(pins, tt.want) {
				t.Fatalf("pins = %v, want %v", pins, tt.want)
			}
		})
	}
}

func TestEditedInputWithoutInputStaysEmpty(t *testing.T) {
	got, err := EditedInput(&model.Cluster{}, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("EditedInput(no input) = %q, %v", got, err)
	}
}
