package reconcile

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// HelmRecord is one revision of a Helm release as Helm stores it in the
// workload cluster: the Secret "sh.helm.release.v1.<release>.v<revision>",
// with its "status" label (deployed, superseded, failed, uninstalling,
// pending-install, pending-upgrade, pending-rollback).
type HelmRecord struct {
	// Name of the Secret.
	Name     string
	Revision int
	Status   string
	// Changed is when Helm last wrote the record (its modifiedAt label,
	// else createdAt, else the Secret's creation time): how long the
	// revision has been in its status.
	Changed time.Time
}

// HelmStorage is the Helm release records of one workload cluster.
type HelmStorage interface {
	// Records are the stored revisions of release in namespace.
	Records(ctx context.Context, namespace, release string) ([]HelmRecord, error)
	// Remove deletes one stored revision (a HelmRecord's Name).
	Remove(ctx context.Context, namespace, name string) error
}

// HelmOperator is what the platform does to a stuck Helm release, with
// Helm itself, in the workload cluster (operations only: provider-helm still
// owns the Release). A HelmStorage may also be a HelmOperator; one that is
// not can only have its stuck record removed (HelmStorage.Remove).
type HelmOperator interface {
	// RollBack makes the release run revision again (the last one Helm
	// deployed), as a new revision. The stuck revision, still pending, is
	// first marked failed so Helm accepts the operation.
	RollBack(ctx context.Context, namespace, release string, stuck, to int) error
	// Uninstall removes a release that never finished its first install
	// (nothing was deployed to go back to), history included, so the next
	// attempt installs cleanly.
	Uninstall(ctx context.Context, namespace, release string) error
	// DeleteFailedHookJobs deletes the failed hook Jobs of release a
	// failed attempt left behind (they block the retry's hook of the same
	// name); it returns their names.
	DeleteFailedHookJobs(ctx context.Context, namespace, release string) ([]string, error)
}

// Workloads reaches workload clusters with the kubeconfig the platform
// holds for them.
type Workloads interface {
	// HelmStorage opens the Helm storage of the workload cluster whose
	// kubeconfig Secret is secretRef ("<namespace>/<name>", as a cluster's
	// status records it), which must be in namespace.
	HelmStorage(ctx context.Context, namespace, secretRef string) (HelmStorage, error)
}

// SubTenantWorkloads opens the target cluster without exposing its credentials
// through RPCs. A per-subtenant reconciler writes only its Capsule Tenant there.
type SubTenantWorkloads interface {
	Client(ctx context.Context, namespace, secretRef string) (client.Client, error)
}

// WorkloadClients opens a client of the workload cluster whose kubeconfig
// Secret is secretRef (in namespace, as Workloads.HelmStorage): the platform
// applies and reads the cluster's Karpenter node groups through it.
type WorkloadClients interface {
	Client(ctx context.Context, namespace, secretRef string) (client.Client, error)
}

// ReadKubeconfig reads a workload cluster's kubeconfig from the Secret a
// cluster's status names (ref: "<namespace>/<name>", or a bare name). The
// Secret is always in the cluster's own namespace: a reference to another
// namespace is refused, so no other namespace is ever read.
func ReadKubeconfig(ctx context.Context, c client.Reader, namespace, ref string) ([]byte, error) {
	if ref == "" {
		return nil, fmt.Errorf("the cluster has no kubeconfig Secret yet")
	}
	name := ref
	if ns, rest, ok := strings.Cut(ref, "/"); ok {
		if ns != namespace {
			return nil, fmt.Errorf("secret %s is not in the cluster's namespace", ref)
		}
		name = rest
	}
	secret := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("secret %s not found", name)
	}
	if apierrors.IsForbidden(err) {
		return nil, fmt.Errorf("the controller may not read secret %s in namespace %s: the %s RoleBinding is missing there (the cloud account's reconcile applies it; --service-account names who it binds)",
			name, namespace, KubeconfigReaderRole)
	}
	if err != nil {
		return nil, fmt.Errorf("secret %s: %w", name, err)
	}
	kubeconfig := secret.Data[crossplaneaws.KubeconfigSecretKey]
	if len(kubeconfig) == 0 {
		return nil, fmt.Errorf("secret %s has no %q key", name, crossplaneaws.KubeconfigSecretKey)
	}
	return kubeconfig, nil
}
