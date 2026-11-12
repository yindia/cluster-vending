package aws

import (
	"context"
	"fmt"
	"slices"

	apiextv1alpha1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// InstallResult is what InstallProviders did with each object.
type InstallResult struct {
	Created, Existing []string
	// Updated: the activation policy, when kinds were added to it.
	Updated []string
}

// InstallProviders creates, in the management cluster, the platform's
// activation policy (ActivationPolicy: only the managed resource kinds it
// uses), the AWS providers' runtime configs (RuntimeConfigs: poll
// intervals, reconcile rate, resources, IRSA role), the AWS providers
// (Providers) and provider-helm (crossplane.HelmProvider). Objects that
// exist are left as they are: the call never overwrites a provider or
// runtime config someone pinned or tuned. The one exception is an existing
// activation policy that lacks kinds this build uses (a server upgrade):
// they are added to it, nothing is removed.
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
			if policy, ok := obj.(*apiextv1alpha1.ManagedResourceActivationPolicy); ok {
				added, err := addActivations(ctx, c, policy)
				if err != nil {
					return out, err
				}
				if added > 0 {
					out.Updated = append(out.Updated, name)
				}
			}
		default:
			return out, fmt.Errorf("create %s: %w", name, err)
		}
	}
	return out, nil
}

// addActivations adds, to the activation policy in the cluster, the kinds
// of want it lacks, and reports how many.
func addActivations(ctx context.Context, c client.Client, want *apiextv1alpha1.ManagedResourceActivationPolicy) (int, error) {
	have := &apiextv1alpha1.ManagedResourceActivationPolicy{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(want), have); err != nil {
		return 0, fmt.Errorf("read activation policy %s: %w", want.GetName(), err)
	}
	added := 0
	for _, a := range want.Spec.Activations {
		if !slices.Contains(have.Spec.Activations, a) {
			have.Spec.Activations = append(have.Spec.Activations, a)
			added++
		}
	}
	if added == 0 {
		return 0, nil
	}
	if err := c.Update(ctx, have); err != nil {
		return 0, fmt.Errorf("add kinds to activation policy %s: %w", want.GetName(), err)
	}
	return added, nil
}
