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

// InstallProviders creates the platform's activation policy
// (ActivationPolicy: only the managed resource kinds it uses), the AWS
// providers (Providers), provider-helm
// (crossplane.HelmProvider) and, when the platform has a management role,
// the AWS providers' DeploymentRuntimeConfig (RuntimeConfig, IRSA) in the
// management cluster. Objects that exist are
// left as they are: the call never overwrites a provider someone pinned
// or configured. Without a management role (local clusters, credentials
// from the ProviderConfig Secret) the providers run with Crossplane's
// default runtime config.
func InstallProviders(ctx context.Context, c client.Client, managementRoleARN string) (InstallResult, error) {
	objects := []client.Object{ActivationPolicy()}
	if managementRoleARN != "" {
		objects = append(objects, RuntimeConfig(managementRoleARN))
	}
	for _, p := range Providers() {
		if managementRoleARN == "" {
			p.Spec.RuntimeConfigReference = nil
		}
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
