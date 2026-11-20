package aws_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// What a deletion that lets the Helm Releases go may leave behind is found
// by the sweep: ClusterLists has the provider-helm Release and
// ProviderConfig.
func TestClusterListsHaveTheHelmKinds(t *testing.T) {
	have := map[string]bool{}
	for _, l := range crossplaneaws.ClusterLists() {
		if u, ok := l.(*unstructured.UnstructuredList); ok {
			have[u.GroupVersionKind().Group+"/"+u.GroupVersionKind().Kind] = true
		}
	}
	for _, want := range []string{"helm.m.crossplane.io/ReleaseList", "helm.m.crossplane.io/ProviderConfigList"} {
		if !have[want] {
			t.Errorf("ClusterLists lacks %s: %v", want, have)
		}
	}
}

// The results Bucket and its settings, and nothing else, are what a deletion
// keeps while the bucket is not emptied.
func TestIsResultsBucketObject(t *testing.T) {
	const prefix = "prod-1a2b3c4d"
	tests := []struct {
		kind, name string
		want       bool
	}{
		{"Bucket", prefix + "-results", true},
		{"BucketPublicAccessBlock", prefix + "-results-public-access", true},
		{"BucketOwnershipControls", prefix + "-results-ownership", true},
		{"BucketServerSideEncryptionConfiguration", prefix + "-results-encryption", true},
		{"BucketPolicy", prefix + "-results-policy", true},
		{"BucketLifecycleConfiguration", prefix + "-results-lifecycle", true},
		{"Cluster", prefix + "-results", false},
		{"Bucket", "other-1a2b3c4d-results", false},
		{"Cluster", prefix, false},
		{"NodeGroup", prefix + "-default", false},
		{"Usage", prefix + "-results-policy", false},
	}
	for _, tt := range tests {
		if got := crossplaneaws.IsResultsBucketObject(prefix, tt.kind, tt.name); got != tt.want {
			t.Errorf("IsResultsBucketObject(%s %s) = %v, want %v", tt.kind, tt.name, got, tt.want)
		}
	}
}
