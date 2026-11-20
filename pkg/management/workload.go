package management

import (
	"context"
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/deprecation"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Workloads is reconcile.Workloads with the kubeconfig Secrets of the
// management cluster, read with c (at the API server: the controller may
// get them in tenant namespaces only). A kubeconfig that would run a
// command is refused (deprecation.RESTConfig).
func Workloads(c client.Reader) reconcile.Workloads { return workloads{c: c} }

type workloads struct{ c client.Reader }

func (w workloads) HelmStorage(ctx context.Context, namespace, secretRef string) (reconcile.HelmStorage, error) {
	kubeconfig, err := reconcile.ReadKubeconfig(ctx, w.c, namespace, secretRef)
	if err != nil {
		return nil, err
	}
	cfg, err := deprecation.RESTConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("workload cluster client: %w", err)
	}
	// Records and, for a recovery, Helm itself (reconcile.HelmOperator).
	return newHelmOps(c, cfg), nil
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
