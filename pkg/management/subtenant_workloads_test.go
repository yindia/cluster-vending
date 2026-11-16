package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const subTenantTestConfig = `apiVersion: v1
kind: Config
current-context: workload
contexts:
- name: workload
  context: {cluster: workload, user: workload}
clusters:
- name: workload
  cluster: {server: "https://workload.invalid"}
users:
- name: workload
  user: {token: dummy-token}
`

func subTenantWorkloadFixture(t *testing.T, funcs interceptor.Funcs) (*subTenantWorkloads, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "workload"}, Data: map[string][]byte{"kubeconfig": []byte(subTenantTestConfig)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).WithInterceptorFuncs(funcs).Build()
	w, ok := SubTenantWorkloads(c).(*subTenantWorkloads)
	if !ok {
		t.Fatal("unexpected workload adapter")
	}
	return w, c
}

func TestSubTenantWorkloadClientReusesClientsAndRefreshesCredentials(t *testing.T) {
	w, c := subTenantWorkloadFixture(t, interceptor.Funcs{})
	builds := 0
	w.newClient = func(cfg *rest.Config) (client.Client, error) {
		builds++
		if cfg.Timeout == 0 || cfg.UserAgent != "cluster-vending/subtenant-controller" {
			t.Fatal("unbounded or unidentified workload client")
		}
		return fake.NewClientBuilder().Build(), nil
	}
	first, err := w.Client(t.Context(), "tenant-acme", "workload")
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.Client(t.Context(), "tenant-acme", "tenant-acme/workload")
	if err != nil || first != second || builds != 1 {
		t.Fatalf("same credential did not reuse client: builds=%d, err=%v", builds, err)
	}
	secret := &corev1.Secret{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: "workload"}, secret); err != nil {
		t.Fatal(err)
	}
	config, err := clientcmd.Load(secret.Data["kubeconfig"])
	if err != nil {
		t.Fatal(err)
	}
	config.AuthInfos["workload"].Token = "refreshed-dummy-token"
	secret.Data["kubeconfig"], err = clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	third, err := w.Client(t.Context(), "tenant-acme", "workload")
	if err != nil || third == first || builds != 2 {
		t.Fatalf("credential rotation was ignored: builds=%d, err=%v", builds, err)
	}
}

func TestSubTenantWorkloadRefCannotReadAnotherTenant(t *testing.T) {
	gets := 0
	w, _ := subTenantWorkloadFixture(t, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		gets++
		return c.Get(ctx, key, obj, opts...)
	}})
	if _, err := w.Client(t.Context(), "tenant-acme", "tenant-other/workload"); err == nil || gets != 0 {
		t.Fatalf("cross-tenant reference was read: gets=%d, err=%v", gets, err)
	}
}

func TestSubTenantWorkloadRejectsExecutableAndLocalFileCredentials(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*clientcmdapi.Config)
	}{
		{"exec", func(c *clientcmdapi.Config) {
			c.AuthInfos["workload"].Exec = &clientcmdapi.ExecConfig{Command: "must-not-run"}
		}},
		{"auth provider", func(c *clientcmdapi.Config) {
			c.AuthInfos["workload"].AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "gcp"}
		}},
		{"token file", func(c *clientcmdapi.Config) { c.AuthInfos["workload"].TokenFile = "/private/must-not-read" }},
		{"client key", func(c *clientcmdapi.Config) { c.AuthInfos["workload"].ClientKey = "/private/must-not-read" }},
		{"CA file", func(c *clientcmdapi.Config) { c.Clusters["workload"].CertificateAuthority = "/private/must-not-read" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, c := subTenantWorkloadFixture(t, interceptor.Funcs{})
			config, err := clientcmd.Load([]byte(subTenantTestConfig))
			if err != nil {
				t.Fatal(err)
			}
			tt.edit(config)
			secret := &corev1.Secret{}
			if err := c.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: "workload"}, secret); err != nil {
				t.Fatal(err)
			}
			secret.Data["kubeconfig"], err = clientcmd.Write(*config)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Update(t.Context(), secret); err != nil {
				t.Fatal(err)
			}
			w.newClient = func(*rest.Config) (client.Client, error) {
				t.Fatal("unsafe credential reached client construction")
				return nil, nil
			}
			if _, err := w.Client(t.Context(), "tenant-acme", "workload"); err == nil {
				t.Fatal("unsafe kubeconfig was accepted")
			}
		})
	}
}

type namespaceDiscovery struct {
	discovery.DiscoveryInterface
	resources []*metav1.APIResourceList
	err       error
}

func (d namespaceDiscovery) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return d.resources, d.err
}

func TestSubTenantNamespaceInventoryCompleteAndFailClosed(t *testing.T) {
	widgets := schema.GroupVersionResource{Group: "example.test", Version: "v1", Resource: "widgets"}
	pods := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	resources := []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: []string{"list", "delete"}},
			{Name: "pods/status", Kind: "Pod", Namespaced: true, Verbs: []string{"list", "delete"}},
			{Name: "readviews", Kind: "ReadView", Namespaced: true, Verbs: []string{"list"}},
			{Name: "namespaces", Kind: "Namespace", Verbs: []string{"list", "delete"}},
		}},
		{GroupVersion: "example.test/v1", APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: []string{"list", "delete"}}}},
	}
	for _, test := range []string{"complete", "partial-discovery", "forbidden-list", "expired-page", "cancelled", "no-resources"} {
		t.Run(test, func(t *testing.T) {
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{widgets: "WidgetList", pods: "PodList"})
			pages := 0
			dyn.PrependReactor("list", "widgets", func(action ktesting.Action) (bool, runtime.Object, error) {
				pages++
				if action.GetNamespace() != "owned" {
					t.Fatal("cross-namespace inventory")
				}
				if test == "forbidden-list" {
					return true, nil, apierrors.NewForbidden(widgets.GroupResource(), "", fmt.Errorf("denied"))
				}
				if pages == 2 && test == "expired-page" {
					return true, nil, apierrors.NewResourceExpired("inventory expired")
				}
				list := &unstructured.UnstructuredList{}
				if pages == 1 {
					list.SetContinue("next")
				}
				list.Items = []unstructured.Unstructured{{Object: map[string]any{"metadata": map[string]any{"name": fmt.Sprintf("item-%d", pages), "namespace": "owned", "uid": fmt.Sprintf("uid-%d", pages)}}}}
				return true, list, nil
			})
			d := namespaceDiscovery{resources: resources}
			if test == "partial-discovery" {
				d.err = fmt.Errorf("one API group unavailable")
			}
			if test == "no-resources" {
				d.resources = nil
			}
			c := &namespaceInventoryClient{dynamic: dyn, discover: func(context.Context) (discovery.DiscoveryInterface, error) { return d, nil }}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test == "cancelled" {
				cancel()
			}
			items, err := c.NamespaceContents(ctx, "owned")
			if test != "complete" {
				if err == nil || items != nil {
					t.Fatalf("partial inventory admitted: %v %v", items, err)
				}
				return
			}
			if err != nil || len(items) != 2 || pages != 2 {
				t.Fatalf("lost API pages: %v %v pages=%d", items, err, pages)
			}
			if items[0].GetKind() != "Widget" || items[0].GetAPIVersion() != "example.test/v1" {
				t.Fatal("object type not retained", items)
			}
			for _, action := range dyn.Actions() {
				if action.GetVerb() != "list" {
					t.Fatal("inventory mutated remote data", action)
				}
			}
		})
	}
}

func TestSubTenantNamespaceDiscoveryTransportHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	transport := inventoryTransport{ctx: ctx, base: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://discovery.invalid/apis", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if response != nil && response.Body != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("discovery ignored caller cancellation", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
