package deprecation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// helmSecretSelector selects the Secrets of deployed Helm v3 releases.
const helmSecretSelector = "owner=helm,status=deployed" //nolint:gosec // a label selector, not a credential

// requestTimeout bounds one call to a workload cluster.
const requestTimeout = 30 * time.Second

// KubeReader reads a workload cluster: discovery, object metadata only
// (the annotations are all a scan needs) and the Helm release Secrets.
// It uses the clients the controller links anyway, not the typed
// clientset of every Kubernetes API.
type KubeReader struct {
	Discovery discovery.DiscoveryInterface
	Metadata  metadata.Interface
	Secrets   client.Reader
}

// NewKubeReader is a KubeReader on the cluster a kubeconfig names. The
// kubeconfig comes from a Secret, so one that would run a command (an
// exec plugin) or load an auth provider is refused: only a token or a
// client certificate is accepted.
func NewKubeReader(kubeconfig []byte) (*KubeReader, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	return NewKubeReaderFor(cfg)
}

// errUnsafeCredentials names the credential kind a config must not carry.
func errUnsafeCredentials(cfg *rest.Config) error {
	switch {
	case cfg.ExecProvider != nil:
		return errors.New("kubeconfig: an exec credential plugin is not allowed")
	case cfg.AuthProvider != nil:
		return errors.New("kubeconfig: an auth provider is not allowed")
	}
	return nil
}

// NewKubeReaderFor is a KubeReader on cfg's cluster.
func NewKubeReaderFor(cfg *rest.Config) (*KubeReader, error) {
	if err := errUnsafeCredentials(cfg); err != nil {
		return nil, err
	}
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = requestTimeout
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	meta, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	secrets, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	return &KubeReader{Discovery: disco, Metadata: meta, Secrets: secrets}, nil
}

// Resources are the preferred version of every resource that can be
// listed. A group whose discovery fails (an aggregated API that is down)
// is left out as long as others answer.
func (k *KubeReader) Resources(_ context.Context) ([]Resource, error) {
	lists, err := discovery.ServerPreferredResources(k.Discovery)
	if err != nil && (len(lists) == 0 || !discovery.IsGroupDiscoveryFailedError(err)) {
		return nil, err
	}
	var out []Resource
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			if slices.Contains(r.Verbs, "list") {
				out = append(out, Resource{Group: gv.Group, Version: gv.Version, Resource: r.Name, Kind: r.Kind})
			}
		}
	}
	return out, nil
}

// ListMeta lists one page of r in every namespace, metadata only.
func (k *KubeReader) ListMeta(ctx context.Context, r Resource, limit int64, token string) ([]Meta, string, error) {
	gvr := schema.GroupVersionResource{Group: r.Group, Version: r.Version, Resource: r.Resource}
	list, err := k.Metadata.Resource(gvr).List(ctx, metav1.ListOptions{Limit: limit, Continue: token})
	if err != nil {
		return nil, "", err
	}
	out := make([]Meta, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, Meta{Namespace: item.Namespace, Name: item.Name, Annotations: item.Annotations})
	}
	return out, list.Continue, nil
}

// ListHelmSecrets lists one page of the deployed Helm v3 release Secrets
// in every namespace.
func (k *KubeReader) ListHelmSecrets(ctx context.Context, limit int64, token string) ([]HelmSecret, string, error) {
	selector, err := labels.Parse(helmSecretSelector)
	if err != nil {
		return nil, "", err
	}
	list := &corev1.SecretList{}
	if err := k.Secrets.List(ctx, list, &client.ListOptions{LabelSelector: selector, Limit: limit, Continue: token}); err != nil {
		return nil, "", err
	}
	out := make([]HelmSecret, 0, len(list.Items))
	for _, item := range list.Items {
		if release, ok := item.Data["release"]; ok {
			out = append(out, HelmSecret{Namespace: item.Namespace, Name: item.Name, Release: release})
		}
	}
	return out, list.Continue, nil
}
