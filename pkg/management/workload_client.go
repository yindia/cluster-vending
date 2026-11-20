package management

import (
	"context"
	"fmt"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/deprecation"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// WorkloadClients is reconcile.WorkloadClients with the kubeconfig Secrets
// of the management cluster, read with c (as Workloads). The client is
// uncached; Karpenter's kinds are read and applied as unstructured.
func WorkloadClients(c client.Reader) reconcile.WorkloadClients { return workloadClients{c: c} }

type workloadClients struct{ c client.Reader }

func (w workloadClients) Client(ctx context.Context, namespace, secretRef string) (client.Client, error) {
	kubeconfig, err := reconcile.ReadKubeconfig(ctx, w.c, namespace, secretRef)
	if err != nil {
		return nil, err
	}
	cfg, err := deprecation.RESTConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		return nil, fmt.Errorf("workload cluster client: %w", err)
	}
	return c, nil
}
