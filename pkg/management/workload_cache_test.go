package management

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func kubeconfigFor(server string) []byte {
	return fmt.Appendf(nil, `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: %s}
users:
- name: u
  user: {token: t}
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
`, server)
}

func kubeconfigSecret(namespace, name, server string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data: map[string][]byte{"kubeconfig": kubeconfigFor(server)}}
}

// countingCache is a cache on management whose builds are counted; each
// build records the REST config it was given.
func countingCache(t *testing.T, management client.Client, o WorkloadCacheOptions) (*WorkloadCache, *atomic.Int32, *atomic.Pointer[rest.Config]) {
	t.Helper()
	cache := NewWorkloadCache(management, o)
	builds, last := &atomic.Int32{}, &atomic.Pointer[rest.Config]{}
	build := cache.build
	cache.build = func(cfg *rest.Config) (client.Client, error) {
		builds.Add(1)
		last.Store(cfg)
		return build(cfg)
	}
	return cache, builds, last
}

func managementWith(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestWorkloadClientIsBuiltOncePerKubeconfig(t *testing.T) {
	management := managementWith(t, kubeconfigSecret("tenant-acme", "prod-kubeconfig", "https://prod.example.test"))
	cache, builds, last := countingCache(t, management, WorkloadCacheOptions{})

	first, err := cache.Client(t.Context(), "tenant-acme", "prod-kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Client(t.Context(), "tenant-acme", "tenant-acme/prod-kubeconfig")
	if err != nil {
		t.Fatal(err)
	}

	if first != second || builds.Load() != 1 {
		t.Errorf("%d builds, same client %t: want one client for one kubeconfig", builds.Load(), first == second)
	}
	if cfg := last.Load(); cfg.QPS != DefaultWorkloadQPS || cfg.Burst != DefaultWorkloadBurst {
		t.Errorf("workload client QPS %v burst %d, want %v and %d", cfg.QPS, cfg.Burst, DefaultWorkloadQPS, DefaultWorkloadBurst)
	}
}

func TestWorkloadClientFollowsItsSecret(t *testing.T) {
	management := managementWith(t, kubeconfigSecret("tenant-acme", "prod-kubeconfig", "https://prod.example.test"))
	cache, builds, last := countingCache(t, management, WorkloadCacheOptions{})
	first, err := cache.Client(t.Context(), "tenant-acme", "prod-kubeconfig")
	if err != nil {
		t.Fatal(err)
	}

	// The ClusterAuth rotates the kubeconfig: a new client, on the new one.
	secret := &corev1.Secret{}
	if err := management.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: "prod-kubeconfig"}, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data["kubeconfig"] = kubeconfigFor("https://rotated.example.test")
	if err := management.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	second, err := cache.Client(t.Context(), "tenant-acme", "prod-kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || builds.Load() != 2 || last.Load().Host != "https://rotated.example.test" {
		t.Errorf("after the Secret changed: %d builds, host %s", builds.Load(), last.Load().Host)
	}

	// The cluster is deleted with its Secret: the client goes with it.
	if err := management.Delete(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Client(t.Context(), "tenant-acme", "prod-kubeconfig"); err == nil {
		t.Fatal("a client for a deleted Secret")
	}
	if n := cache.Len(); n != 0 {
		t.Errorf("%d cached clients after the Secret was deleted, want 0", n)
	}
}

func TestWorkloadClientCacheIsCapped(t *testing.T) {
	var objects []client.Object
	for i := range 3 {
		objects = append(objects, kubeconfigSecret("tenant-acme", fmt.Sprintf("c%d-kubeconfig", i), fmt.Sprintf("https://c%d.example.test", i)))
	}
	cache, builds, _ := countingCache(t, managementWith(t, objects...), WorkloadCacheOptions{MaxClients: 2})
	now := time.Unix(1_800_000_000, 0)
	cache.now = func() time.Time { return now }
	use := func(name string) {
		t.Helper()
		now = now.Add(time.Second)
		if _, err := cache.Client(t.Context(), "tenant-acme", name); err != nil {
			t.Fatal(err)
		}
	}

	use("c0-kubeconfig")
	use("c1-kubeconfig")
	use("c0-kubeconfig") // c1 is now the least recently used
	use("c2-kubeconfig") // evicts c1
	use("c0-kubeconfig") // still cached

	if cache.Len() != 2 || builds.Load() != 3 {
		t.Fatalf("%d cached, %d builds; want 2 and 3", cache.Len(), builds.Load())
	}
	use("c1-kubeconfig") // built again
	if builds.Load() != 4 {
		t.Errorf("the evicted client was not rebuilt: %d builds", builds.Load())
	}
}

// A client unused for longer than the idle time is dropped at the next
// call: a cluster gone without another call for it leaves nothing behind.
func TestWorkloadClientCacheDropsIdleClients(t *testing.T) {
	cache, _, _ := countingCache(t, managementWith(t,
		kubeconfigSecret("tenant-acme", "a-kubeconfig", "https://a.example.test"),
		kubeconfigSecret("tenant-acme", "b-kubeconfig", "https://b.example.test")), WorkloadCacheOptions{IdleTimeout: time.Hour})
	now := time.Unix(1_800_000_000, 0)
	cache.now = func() time.Time { return now }
	if _, err := cache.Client(t.Context(), "tenant-acme", "a-kubeconfig"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := cache.Client(t.Context(), "tenant-acme", "b-kubeconfig"); err != nil {
		t.Fatal(err)
	}
	if cache.Len() != 1 {
		t.Errorf("%d cached clients, want the idle one dropped", cache.Len())
	}
}
