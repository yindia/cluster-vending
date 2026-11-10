package kube_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/kube"
)

const twoContexts = `apiVersion: v1
kind: Config
clusters:
- name: dev
  cluster: {server: "https://dev.example:6443"}
- name: prod
  cluster: {server: "%s"}
users:
- name: u
  user: {token: t}
contexts:
- name: dev
  context: {cluster: dev, user: u}
- name: prod
  context: {cluster: prod, user: u}
current-context: dev
`

func writeKubeconfig(t *testing.T, prodServer string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(strings.Replace(twoContexts, "%s", prodServer, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRESTConfigSelectsContext(t *testing.T) {
	path := writeKubeconfig(t, "https://prod.example:6443")
	tests := []struct {
		context, wantHost string
	}{
		{"", "https://dev.example:6443"}, // current context
		{"prod", "https://prod.example:6443"},
	}
	for _, tt := range tests {
		cfg, err := kube.RESTConfig(path, tt.context)
		if err != nil {
			t.Fatalf("RESTConfig(%q): %v", tt.context, err)
		}
		if cfg.Host != tt.wantHost {
			t.Errorf("context %q: host = %s, want %s", tt.context, cfg.Host, tt.wantHost)
		}
		if cfg.QPS != kube.DefaultQPS || cfg.Burst != kube.DefaultBurst || !strings.HasPrefix(cfg.UserAgent, "cluster-vending/") {
			t.Errorf("client settings not applied: %+v", cfg)
		}
	}
}

func TestRESTConfigErrors(t *testing.T) {
	path := writeKubeconfig(t, "https://prod.example:6443")
	if _, err := kube.RESTConfig(path, "missing"); err == nil {
		t.Error("want error for an unknown context")
	}
	if _, err := kube.RESTConfig(filepath.Join(t.TempDir(), "none"), ""); err == nil {
		t.Error("want error for a missing kubeconfig file")
	}
}

func TestTenantNamespace(t *testing.T) {
	if got := kube.TenantNamespace("acme"); got != "tenant-acme" {
		t.Errorf("TenantNamespace = %q", got)
	}
}

// fakeAPIServer answers discovery for one Crossplane group and GETs of one
// VPC, enough to show the client decodes typed managed resources.
func fakeAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"kind": "APIVersions", "versions": []string{"v1"}})
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		v := map[string]any{"groupVersion": "ec2.aws.m.upbound.io/v1beta1", "version": "v1beta1"}
		reply(w, map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{
			map[string]any{"name": "ec2.aws.m.upbound.io", "versions": []any{v}, "preferredVersion": v},
		}})
	})
	mux.HandleFunc("/apis/ec2.aws.m.upbound.io/v1beta1", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": "ec2.aws.m.upbound.io/v1beta1",
			"resources": []any{map[string]any{"name": "vpcs", "singularName": "vpc", "namespaced": true, "kind": "VPC", "verbs": []string{"get"}}}})
	})
	mux.HandleFunc("/apis/ec2.aws.m.upbound.io/v1beta1/namespaces/tenant-acme/vpcs/main", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"apiVersion": "ec2.aws.m.upbound.io/v1beta1", "kind": "VPC",
			"metadata": map[string]any{"name": "main", "namespace": "tenant-acme"},
			"spec":     map[string]any{"forProvider": map[string]any{"region": "us-east-1"}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClientReadsTypedManagedResources(t *testing.T) {
	srv := fakeAPIServer(t)
	c, err := kube.Connect(writeKubeconfig(t, srv.URL), "prod")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	vpc := &awsec2.VPC{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: kube.TenantNamespace("acme"), Name: "main"}, vpc); err != nil {
		t.Fatalf("Get VPC: %v", err)
	}
	if r := vpc.Spec.ForProvider.Region; r == nil || *r != "us-east-1" {
		t.Errorf("region = %v", r)
	}
}

func TestSchemeKnowsPlatformTypes(t *testing.T) {
	s, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Version: "v1", Kind: "Namespace"},
		{Group: "pkg.crossplane.io", Version: "v1", Kind: "Provider"},
		{Group: "pkg.crossplane.io", Version: "v1beta1", Kind: "DeploymentRuntimeConfig"},
		{Group: "aws.m.upbound.io", Version: "v1beta1", Kind: "ProviderConfig"},
		{Group: "ec2.aws.m.upbound.io", Version: "v1beta1", Kind: "VPC"},
		{Group: "eks.aws.m.upbound.io", Version: "v1beta1", Kind: "Cluster"},
		{Group: "iam.aws.m.upbound.io", Version: "v1beta1", Kind: "Role"},
	} {
		if !s.Recognizes(gvk) {
			t.Errorf("scheme does not know %s", gvk)
		}
	}
}
