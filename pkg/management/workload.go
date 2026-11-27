package management

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Workloads is reconcile.Workloads with the kubeconfig Secrets of the
// management cluster, read with c (at the API server: the controller may
// get them in tenant namespaces only), on a WorkloadCache of its own. A
// kubeconfig that would run a command is refused (deprecation.RESTConfig).
func Workloads(c client.Reader) reconcile.Workloads {
	return NewWorkloadCache(c, WorkloadCacheOptions{})
}

// HelmStorage is the Helm release records of the workload cluster, and
// for a recovery Helm itself (reconcile.HelmOperator), on its cached
// client.
func (w *WorkloadCache) HelmStorage(ctx context.Context, namespace, secretRef string) (reconcile.HelmStorage, error) {
	entry, err := w.entry(ctx, namespace, secretRef)
	if err != nil {
		return nil, err
	}
	return newHelmOps(entry.client, rest.CopyConfig(entry.cfg)), nil
}

// Helm's labels on a release record (helm.sh/helm/v4
// pkg/storage/driver/secrets.go).
const (
	helmLabelOwner      = "owner"
	helmLabelName       = "name"
	helmLabelStatus     = "status"
	helmLabelVersion    = "version"
	helmLabelModifiedAt = "modifiedAt"
	helmLabelCreatedAt  = "createdAt"
	helmOwner           = "helm"
)

// HelmStorageOf is the Helm release records (Secrets) of the cluster c
// talks to.
func HelmStorageOf(c client.Client) reconcile.HelmStorage { return helmStorage{c: c} }

type helmStorage struct{ c client.Client }

func (h helmStorage) Records(ctx context.Context, namespace, release string) ([]reconcile.HelmRecord, error) {
	list := &corev1.SecretList{}
	if err := h.c.List(ctx, list, client.InNamespace(namespace),
		client.MatchingLabels{helmLabelOwner: helmOwner, helmLabelName: release}); err != nil {
		return nil, fmt.Errorf("list the Helm records of %s: %w", release, err)
	}
	out := make([]reconcile.HelmRecord, 0, len(list.Items))
	for _, s := range list.Items {
		revision, err := strconv.Atoi(s.Labels[helmLabelVersion])
		if err != nil {
			continue // not a record Helm wrote
		}
		out = append(out, reconcile.HelmRecord{Name: s.Name, Revision: revision, Status: s.Labels[helmLabelStatus],
			Changed: recordChanged(&s)})
	}
	return out, nil
}

// recordChanged is when Helm last wrote a record: its modifiedAt label
// (set on every update), else createdAt, else the Secret's creation time.
func recordChanged(s *corev1.Secret) time.Time {
	for _, label := range []string{helmLabelModifiedAt, helmLabelCreatedAt} {
		if seconds, err := strconv.ParseInt(s.Labels[label], 10, 64); err == nil && seconds > 0 {
			return time.Unix(seconds, 0)
		}
	}
	return s.CreationTimestamp.Time
}

func (h helmStorage) Remove(ctx context.Context, namespace, name string) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	return client.IgnoreNotFound(h.c.Delete(ctx, secret))
}
