package management

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/deprecation"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Defaults of a WorkloadCache.
const (
	// DefaultWorkloadQPS and DefaultWorkloadBurst are the client-side rate
	// limits of one workload cluster's client: above client-go's 5 and 10,
	// which a component rollout's reads exceed, and far below the
	// management client's, since each workload cluster has its own API
	// server and a pass reads a handful of objects there.
	DefaultWorkloadQPS   = 20
	DefaultWorkloadBurst = 40
	// DefaultWorkloadClients caps the cached clients: the ~100 clusters of
	// one management cluster with room to spare. Past it the least
	// recently used goes.
	DefaultWorkloadClients = 256
	// DefaultWorkloadIdle drops a client unused this long: a deleted
	// cluster's client does not stay until the cap pushes it out.
	DefaultWorkloadIdle = 30 * time.Minute
)

// WorkloadCacheOptions tune a WorkloadCache; a zero field is its default.
type WorkloadCacheOptions struct {
	QPS         float32
	Burst       int
	MaxClients  int
	IdleTimeout time.Duration
}

// WorkloadCache is reconcile.WorkloadClients and reconcile.Workloads with
// one client per workload cluster, kept between calls. A client (and its
// REST mapper, which learns the cluster's API groups by discovery) is built
// once per kubeconfig: every call reads the kubeconfig Secret (a GET at the
// management API server, so a rotated or revoked kubeconfig is seen at
// once) and reuses the client while the kubeconfig is the same. A changed
// kubeconfig replaces the client; a Secret that cannot be read (deleted
// with its cluster) drops it. Safe for concurrent use.
type WorkloadCache struct {
	c       client.Reader
	options WorkloadCacheOptions
	// build makes the client of a REST config (tests count it).
	build func(*rest.Config) (client.Client, error)
	now   func() time.Time

	mu      sync.Mutex
	clients map[string]*workloadClient
}

// workloadClient is one cached client and what it was built from.
type workloadClient struct {
	digest [sha256.Size]byte
	cfg    *rest.Config
	client client.Client
	used   time.Time
}

// NewWorkloadCache is a WorkloadCache reading kubeconfig Secrets with c
// (at the API server: the controller may get them in tenant namespaces
// only).
func NewWorkloadCache(c client.Reader, o WorkloadCacheOptions) *WorkloadCache {
	if o.QPS <= 0 {
		o.QPS = DefaultWorkloadQPS
	}
	if o.Burst <= 0 {
		o.Burst = DefaultWorkloadBurst
	}
	if o.MaxClients <= 0 {
		o.MaxClients = DefaultWorkloadClients
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultWorkloadIdle
	}
	return &WorkloadCache{c: c, options: o, build: newWorkloadClient, now: time.Now, clients: map[string]*workloadClient{}}
}

// newWorkloadClient is an uncached client of the workload cluster with
// the built-in kinds; Karpenter's kinds are read and applied as
// unstructured.
func newWorkloadClient(cfg *rest.Config) (client.Client, error) {
	return client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
}

// WorkloadClients is reconcile.WorkloadClients on a cache of its own.
func WorkloadClients(c client.Reader) reconcile.WorkloadClients {
	return NewWorkloadCache(c, WorkloadCacheOptions{})
}

// Client is the workload cluster's client, from the kubeconfig in the
// Secret secretRef of namespace.
func (w *WorkloadCache) Client(ctx context.Context, namespace, secretRef string) (client.Client, error) {
	entry, err := w.entry(ctx, namespace, secretRef)
	if err != nil {
		return nil, err
	}
	return entry.client, nil
}

// Len is how many clients are cached.
func (w *WorkloadCache) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.clients)
}

func (w *WorkloadCache) entry(ctx context.Context, namespace, secretRef string) (*workloadClient, error) {
	key := namespace + "/" + secretRef[strings.Index(secretRef, "/")+1:]
	kubeconfig, err := reconcile.ReadKubeconfig(ctx, w.c, namespace, secretRef)
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.dropIdle(now)
	if err != nil {
		delete(w.clients, key)
		return nil, err
	}
	digest := sha256.Sum256(kubeconfig)
	if cached, ok := w.clients[key]; ok && cached.digest == digest {
		cached.used = now
		return cached, nil
	}
	cfg, err := deprecation.RESTConfig(kubeconfig)
	if err != nil {
		delete(w.clients, key)
		return nil, err
	}
	cfg.QPS, cfg.Burst = w.options.QPS, w.options.Burst
	c, err := w.build(cfg)
	if err != nil {
		delete(w.clients, key)
		return nil, fmt.Errorf("workload cluster client: %w", err)
	}
	entry := &workloadClient{digest: digest, cfg: cfg, client: c, used: now}
	w.clients[key] = entry
	w.dropOverCap()
	return entry, nil
}

// dropIdle drops the clients unused for longer than the idle timeout.
func (w *WorkloadCache) dropIdle(now time.Time) {
	for key, cached := range w.clients {
		if now.Sub(cached.used) > w.options.IdleTimeout {
			delete(w.clients, key)
		}
	}
}

// dropOverCap drops the least recently used clients past the cap.
func (w *WorkloadCache) dropOverCap() {
	for len(w.clients) > w.options.MaxClients {
		oldest := ""
		for key, cached := range w.clients {
			if oldest == "" || cached.used.Before(w.clients[oldest].used) {
				oldest = key
			}
		}
		delete(w.clients, oldest)
	}
}
