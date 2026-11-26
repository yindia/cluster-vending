package aws

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apiextv1alpha1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1alpha1"
	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// InstallResult is what InstallProviders did with each object.
type InstallResult struct {
	Created, Existing []string
	// Updated: the activation policy, when kinds were added to it.
	Updated []string
}

// InstallProviders creates, in the management cluster, the platform's
// activation policy (ActivationPolicy: only the managed resource kinds it
// uses), the providers' runtime configs (RuntimeConfigs: poll intervals,
// reconcile rate, resources, replicas and leader election, IRSA role), the
// AWS providers (Providers) and provider-helm (HelmProvider). Objects that
// exist are left as they are: the call never overwrites a provider or
// runtime config someone pinned or tuned. Two exceptions, both for a
// server upgrade: an existing activation policy that lacks kinds this
// build uses gets them added (nothing is removed), and provider-helm's
// runtime config (RuntimeConfigHelmName) still polling at the 15s earlier
// builds gave it is moved to the poll rt says (DefaultHelmPoll): only that
// argument changes, and a poll someone tuned to any other value is kept.
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
	objects = append(objects, HelmProvider())
	var out InstallResult
	for _, obj := range objects {
		name := fmt.Sprintf("%s/%s", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
		err := c.Create(ctx, obj)
		switch {
		case err == nil:
			out.Created = append(out.Created, name)
		case apierrors.IsAlreadyExists(err):
			out.Existing = append(out.Existing, name)
			updated := false
			switch o := obj.(type) {
			case *apiextv1alpha1.ManagedResourceActivationPolicy:
				added, err := addActivations(ctx, c, o)
				if err != nil {
					return out, err
				}
				updated = added > 0
			case *pkgv1beta1.DeploymentRuntimeConfig:
				if o.Name == RuntimeConfigHelmName {
					if updated, err = migrateHelmPoll(ctx, c, o); err != nil {
						return out, err
					}
				}
			}
			if updated {
				out.Updated = append(out.Updated, name)
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

// migrateHelmPoll moves provider-helm's existing runtime config from the
// legacy 15s poll to the one want renders, and reports whether it did. Only
// the --poll argument of the provider container changes; a config without
// the legacy value (tuned, or already moved) is left as it is.
func migrateHelmPoll(ctx context.Context, c client.Client, want *pkgv1beta1.DeploymentRuntimeConfig) (bool, error) {
	wantArg := pollArg(want)
	if wantArg == "" {
		return false, nil
	}
	have := &pkgv1beta1.DeploymentRuntimeConfig{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(want), have); err != nil {
		return false, fmt.Errorf("read runtime config %s: %w", want.GetName(), err)
	}
	legacy := "--poll=" + legacyHelmPoll.String()
	if have.Spec.DeploymentTemplate == nil || have.Spec.DeploymentTemplate.Spec == nil {
		return false, nil
	}
	containers := have.Spec.DeploymentTemplate.Spec.Template.Spec.Containers
	changed := false
	for i := range containers {
		if containers[i].Name != runtimeContainer {
			continue
		}
		args := slices.Clone(containers[i].Args)
		for j, a := range args {
			if a == legacy {
				args[j], changed = wantArg, true
			}
		}
		containers[i].Args = args
	}
	if !changed {
		return false, nil
	}
	if err := c.Update(ctx, have); err != nil {
		return false, fmt.Errorf("move runtime config %s to %s: %w", want.GetName(), wantArg, err)
	}
	return true, nil
}

// pollArg is the --poll argument of a rendered runtime config's provider
// container; "" when it has none.
func pollArg(rc *pkgv1beta1.DeploymentRuntimeConfig) string {
	if rc.Spec.DeploymentTemplate == nil || rc.Spec.DeploymentTemplate.Spec == nil {
		return ""
	}
	for _, ctr := range rc.Spec.DeploymentTemplate.Spec.Template.Spec.Containers {
		if ctr.Name != runtimeContainer {
			continue
		}
		for _, a := range ctr.Args {
			if strings.HasPrefix(a, "--poll=") {
				return a
			}
		}
	}
	return ""
}
