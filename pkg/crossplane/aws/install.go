package aws

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// InstallResult is what InstallProviders did with each object.
type InstallResult struct {
	Created, Existing []string
}

// InstallProviders creates, in the management cluster, the platform's
// activation policy (ActivationPolicy: only the managed resource kinds it
// uses), the AWS providers' runtime configs (RuntimeConfigs: poll
// intervals, reconcile rate, resources, IRSA role), the AWS providers
// (Providers) and provider-helm (crossplane.HelmProvider). Objects that
// exist are left as they are: the call never overwrites a provider or
// runtime config someone pinned or tuned.
func InstallProviders(ctx context.Context, c client.Client, rt ProviderRuntime) (InstallResult, error) {
	objects := []client.Object{ActivationPolicy()}
	configs, err := RuntimeConfigs(rt)
	if err != nil {
		return InstallResult{}, err
	}
	for _, config := range configs {
		objects = append(objects, config)
	}
	for _, p := range Providers() {
		objects = append(objects, p)
	}
	objects = append(objects, crossplane.HelmProvider())
	var out InstallResult
	for _, obj := range objects {
		name := fmt.Sprintf("%s/%s", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
		err := c.Create(ctx, obj)
		switch {
		case err == nil:
			out.Created = append(out.Created, name)
		case apierrors.IsAlreadyExists(err):
			out.Existing = append(out.Existing, name)
		default:
			return out, fmt.Errorf("create %s: %w", name, err)
		}
	}
	return out, nil
}
