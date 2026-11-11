package kube_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/kube"
)

// Reads only use the informers registered for the cached kinds: any other
// kind fails instead of starting an informer, and Secrets bypass the cache
// (no Secret informer ever starts).
func TestManagerCachesOnlyTheGivenKinds(t *testing.T) {
	vpc := schema.GroupVersionKind{Group: "ec2.aws.m.upbound.io", Version: "v1beta1", Kind: "VPC"}
	// An API server that only answers discovery: the manager is never
	// started, so any other request is a read that bypassed the cache.
	var direct []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api":
			_, _ = w.Write([]byte(`{"kind":"APIVersions","versions":["v1"]}`))
		case "/apis":
			_, _ = w.Write([]byte(`{"kind":"APIGroupList","groups":[{"name":"ec2.aws.m.upbound.io","versions":[{"groupVersion":"ec2.aws.m.upbound.io/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"ec2.aws.m.upbound.io/v1beta1","version":"v1beta1"}}]}`))
		case "/api/v1":
			_, _ = w.Write([]byte(`{"kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"secrets","namespaced":true,"kind":"Secret"},{"name":"configmaps","namespaced":true,"kind":"ConfigMap"}]}`))
		case "/apis/ec2.aws.m.upbound.io/v1beta1":
			_, _ = w.Write([]byte(`{"kind":"APIResourceList","groupVersion":"ec2.aws.m.upbound.io/v1beta1","resources":[{"name":"vpcs","namespaced":true,"kind":"VPC"}]}`))
		default:
			direct = append(direct, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	mgr, err := kube.NewManager(&rest.Config{Host: api.URL}, []schema.GroupVersionKind{vpc}, kube.LeaderElection{})
	if err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "tenant-acme", Name: "x"}
	var notCached *cache.ErrResourceNotCached

	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if err := mgr.GetClient().Get(t.Context(), key, cm); !errors.As(err, &notCached) {
		t.Errorf("uncached kind: %v, want ErrResourceNotCached (no informer started on demand)", err)
	}
	if len(direct) != 0 {
		t.Errorf("the uncached read reached the API server: %v", direct)
	}
	// A Secret read goes to the API server, never to the cache.
	err = mgr.GetClient().Get(t.Context(), key, &corev1.Secret{})
	if err == nil || errors.As(err, &notCached) || len(direct) != 1 || direct[0] != "/api/v1/namespaces/tenant-acme/secrets/x" {
		t.Errorf("secret read: %v, requests %v; want one direct request: Secrets are not cached", err, direct)
	}
}
