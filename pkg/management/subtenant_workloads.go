package management

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/deprecation"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// SubTenantWorkloads reuses one workload client per namespaced kubeconfig while
// its embedded credentials stay unchanged. Tokens refreshed by ClusterAuth
// replace the cached client; Secrets and credentials are never exposed by RPCs.
func SubTenantWorkloads(reader client.Reader) reconcile.SubTenantWorkloads {
	return &subTenantWorkloads{reader: reader, clients: map[string]subTenantClient{}, newClient: newSubTenantRemoteClient}
}

// namespaceInventoryClient retains normal Tenant operations while providing a
// fresh, complete read-only discovery scan for guarded namespace offboarding.
type namespaceInventoryClient struct {
	client.Client
	dynamic  dynamic.Interface
	discover func(context.Context) (discovery.DiscoveryInterface, error)
}

func newSubTenantRemoteClient(cfg *rest.Config) (client.Client, error) {
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme, HTTPClient: httpClient})
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	return &namespaceInventoryClient{Client: c, dynamic: dyn, discover: func(ctx context.Context) (discovery.DiscoveryInterface, error) {
		// Discovery's legacy methods do not accept a context. Bind all HTTP
		// requests to this scan so cancellation also interrupts discovery.
		bounded := *httpClient
		bounded.Transport = inventoryTransport{ctx: ctx, base: httpClient.Transport}
		return discovery.NewDiscoveryClientForConfigAndClient(cfg, &bounded)
	}}, nil
}

type inventoryTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t inventoryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(request.Clone(t.ctx))
}

// NamespaceContents never treats a partial discovery/list result as empty.
// No inventory is cached between preflight and the final deletion recheck.
func (c *namespaceInventoryClient) NamespaceContents(ctx context.Context, namespace string) ([]unstructured.Unstructured, error) {
	if namespace == "" {
		return nil, fmt.Errorf("namespace inventory requires an explicit namespace")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	disco, err := c.discover(ctx)
	if err != nil {
		return nil, fmt.Errorf("namespace resource discovery unavailable: %w", err)
	}
	resources, err := disco.ServerPreferredNamespacedResources()
	if err != nil {
		return nil, fmt.Errorf("namespace resource discovery is incomplete: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := map[schema.GroupResource]bool{}
	var result []unstructured.Unstructured
	for _, group := range resources {
		if group == nil {
			return nil, fmt.Errorf("namespace resource discovery returned an incomplete group")
		}
		gv, err := schema.ParseGroupVersion(group.GroupVersion)
		if err != nil {
			return nil, err
		}
		for _, resource := range group.APIResources {
			if !resource.Namespaced || strings.Contains(resource.Name, "/") || !slices.Contains(resource.Verbs, "list") || !slices.Contains(resource.Verbs, "delete") {
				continue
			}
			gvr := gv.WithResource(resource.Name)
			if seen[gvr.GroupResource()] {
				continue
			}
			seen[gvr.GroupResource()] = true
			continuation := ""
			pages := map[string]bool{}
			for {
				list, err := c.dynamic.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 500, Continue: continuation})
				if err != nil {
					return nil, fmt.Errorf("cannot completely inspect %s in namespace %s: %w", gvr.GroupResource(), namespace, err)
				}
				if list == nil {
					return nil, fmt.Errorf("empty inventory response for %s", gvr.GroupResource())
				}
				for _, item := range list.Items {
					if item.GetNamespace() != namespace || item.GetName() == "" || item.GetUID() == "" {
						return nil, fmt.Errorf("unverifiable object identity in %s", gvr.GroupResource())
					}
					item.SetAPIVersion(gv.String())
					item.SetKind(resource.Kind)
					result = append(result, item)
					if len(result) > 10000 {
						return nil, fmt.Errorf("namespace inventory exceeds safe scan budget; deletion is blocked")
					}
				}
				continuation = list.GetContinue()
				if continuation == "" {
					break
				}
				if pages[continuation] {
					return nil, fmt.Errorf("repeated inventory cursor for %s", gvr.GroupResource())
				}
				pages[continuation] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("namespace discovery found no inspectable resources")
	}
	return result, ctx.Err()
}

type subTenantClient struct {
	digest [sha256.Size]byte
	client client.Client
}

type subTenantWorkloads struct {
	reader    client.Reader
	mu        sync.Mutex
	clients   map[string]subTenantClient
	newClient func(*rest.Config) (client.Client, error)
}

func (w *subTenantWorkloads) Client(ctx context.Context, namespace, secretRef string) (client.Client, error) {
	if w.reader == nil {
		return nil, fmt.Errorf("management kubeconfig reader is unavailable")
	}
	raw, err := reconcile.ReadKubeconfig(ctx, w.reader, namespace, secretRef)
	if err != nil {
		return nil, err
	}
	// Validate before constructing a REST config: external file references must
	// never make a workload Secret read the management process's local files.
	config, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("workload kubeconfig is invalid")
	}
	for _, auth := range config.AuthInfos {
		if auth.Exec != nil || auth.AuthProvider != nil || auth.ClientCertificate != "" || auth.ClientKey != "" || auth.TokenFile != "" {
			return nil, fmt.Errorf("workload kubeconfig must use embedded credentials without plugins or local file references")
		}
	}
	for _, cluster := range config.Clusters {
		if cluster.CertificateAuthority != "" {
			return nil, fmt.Errorf("workload kubeconfig must embed its certificate authority")
		}
	}
	name := secretRef
	if _, short, ok := strings.Cut(secretRef, "/"); ok {
		name = short
	}
	key, digest := namespace+"/"+name, sha256.Sum256(raw)
	w.mu.Lock()
	defer w.mu.Unlock()
	if cached, ok := w.clients[key]; ok && cached.digest == digest {
		return cached.client, nil
	}
	cfg, err := deprecation.RESTConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("workload kubeconfig is not usable: %w", err)
	}
	cfg.UserAgent = "cluster-vending/subtenant-controller"
	cfg.QPS, cfg.Burst = 20, 40
	c, err := w.newClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("construct workload cluster client: %w", err)
	}
	// Bound retained clients when clusters are deleted or kubeconfig names change.
	if _, exists := w.clients[key]; !exists && len(w.clients) >= 256 {
		for old := range w.clients {
			delete(w.clients, old)
			break
		}
	}
	w.clients[key] = subTenantClient{digest: digest, client: c}
	return c, nil
}
