package aws

import (
	"context"
	"fmt"
	"slices"

	apiextv1alpha1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1alpha1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// InstallResult is what InstallProviders did with each object.
type InstallResult struct {
	Created, Existing []string
	// Updated: objects brought to this build's settings (the activation
	// policy with kinds added, runtime configs with new tuning, providers
	// moved to their own runtime config).
	Updated []string
	// Kept: fields left as another manager (an operator) set them, as
	// "kind/name (fields)".
	Kept []string
	// Deleted: the legacy shared runtime config, once nothing used it.
	Deleted []string
}

// InstallProviders brings, in the management cluster, the platform's
// activation policy (ActivationPolicy: only the managed resource kinds it
// uses), the providers' runtime configs (RuntimeConfigs: one per provider,
// with poll interval, reconcile rate, resources, replicas, leader election
// and the IRSA role), the AWS providers (Providers) and provider-helm
// (HelmProvider) to rt, on every start:
//
//   - The runtime configs are server-side applied as the platform's field
//     owner (kube.FieldOwner): a new build's tuning (or the platform
//     config's providers section) reaches existing installs, while a field
//     another manager owns (an operator's kubectl edit or own apply) is
//     left as it is (InstallResult.Kept).
//   - A missing Provider is created; an existing one only has its
//     runtimeConfigRef pointed at its own runtime config, unless another
//     manager set it. Its package (version) is never changed: an operator
//     may have pinned it.
//   - The activation policy gains the kinds this build uses (nothing is
//     removed).
//   - The runtime config earlier builds shared (LegacyRuntimeConfigName)
//     is deleted once no Provider refers to it.
func InstallProviders(ctx context.Context, c client.Client, rt ProviderRuntime) (InstallResult, error) {
	configs, err := RuntimeConfigs(rt)
	if err != nil {
		return InstallResult{}, err
	}
	var out InstallResult
	if err := installActivationPolicy(ctx, c, &out); err != nil {
		return out, err
	}
	for _, rc := range configs {
		if err := applyRuntimeConfig(ctx, c, rc, &out); err != nil {
			return out, err
		}
	}
	providers := Providers()
	providers = append(providers, HelmProvider())
	for _, p := range providers {
		if err := installProvider(ctx, c, p, &out); err != nil {
			return out, err
		}
	}
	err = deleteLegacyRuntimeConfig(ctx, c, &out)
	return out, err
}

func installActivationPolicy(ctx context.Context, c client.Client, out *InstallResult) error {
	policy := ActivationPolicy()
	name := "ManagedResourceActivationPolicy/" + policy.Name
	err := c.Create(ctx, policy, client.FieldOwner(kube.FieldOwner))
	switch {
	case err == nil:
		out.Created = append(out.Created, name)
		return nil
	case !apierrors.IsAlreadyExists(err):
		return fmt.Errorf("create %s: %w", name, err)
	}
	out.Existing = append(out.Existing, name)
	added, err := addActivations(ctx, c, policy)
	if added > 0 {
		out.Updated = append(out.Updated, name)
	}
	return err
}

func applyRuntimeConfig(ctx context.Context, c client.Client, rc *pkgv1beta1.DeploymentRuntimeConfig, out *InstallResult) error {
	name := "DeploymentRuntimeConfig/" + rc.Name
	body, err := runtimeConfigBody(c, rc)
	if err != nil {
		return err
	}
	res, err := applyKeeping(ctx, c, body, runtimeConfigFields())
	if err != nil {
		return err
	}
	out.record(name, res)
	return nil
}

// installProvider creates the Provider p, or points an existing one at
// its runtime config (a merge patch of runtimeConfigRef only, as
// kube.FieldOwner), unless another manager set that reference.
func installProvider(ctx context.Context, c client.Client, p *pkgv1.Provider, out *InstallResult) error {
	name := "Provider/" + p.Name
	err := c.Create(ctx, p.DeepCopy(), client.FieldOwner(kube.FieldOwner))
	switch {
	case err == nil:
		out.Created = append(out.Created, name)
		return nil
	case !apierrors.IsAlreadyExists(err):
		return fmt.Errorf("create %s: %w", name, err)
	}
	out.Existing = append(out.Existing, name)
	have := &pkgv1.Provider{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(p), have); err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	want := p.Spec.RuntimeConfigReference.Name
	if ownedByAny(othersFields(have.GetManagedFields()), providerRefKeys, false) {
		if ref := have.Spec.RuntimeConfigReference; ref == nil || ref.Name != want {
			out.Kept = append(out.Kept, keptName(name, []string{"runtimeConfigRef"}))
		}
		return nil
	}
	if ref := have.Spec.RuntimeConfigReference; ref != nil && ref.Name == want {
		return nil
	}
	patch := client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"spec":{"runtimeConfigRef":{"name":%q}}}`, want))
	if err := c.Patch(ctx, have, patch, client.FieldOwner(kube.FieldOwner)); err != nil {
		return fmt.Errorf("point %s at runtime config %s: %w", name, want, err)
	}
	out.Updated = append(out.Updated, name)
	return nil
}

// record files an applied object under what happened to it.
func (out *InstallResult) record(name string, res applyResult) {
	switch {
	case res.created:
		out.Created = append(out.Created, name)
	case res.changed:
		out.Existing = append(out.Existing, name)
		out.Updated = append(out.Updated, name)
	default:
		out.Existing = append(out.Existing, name)
	}
	if len(res.kept) > 0 {
		out.Kept = append(out.Kept, keptName(name, res.kept))
	}
}

// deleteLegacyRuntimeConfig deletes the runtime config earlier builds
// shared, when it is the platform's (its labels) and no Provider refers
// to it any more.
func deleteLegacyRuntimeConfig(ctx context.Context, c client.Client, out *InstallResult) error {
	legacy := &pkgv1beta1.DeploymentRuntimeConfig{}
	err := c.Get(ctx, client.ObjectKey{Name: LegacyRuntimeConfigName}, legacy)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read runtime config %s: %w", LegacyRuntimeConfigName, err)
	}
	if legacy.Labels[crossplane.LabelManagedBy] != crossplane.Labels("")[crossplane.LabelManagedBy] {
		return nil
	}
	providers := &pkgv1.ProviderList{}
	if err := c.List(ctx, providers); err != nil {
		return fmt.Errorf("list providers: %w", err)
	}
	for _, p := range providers.Items {
		if ref := p.Spec.RuntimeConfigReference; ref != nil && ref.Name == LegacyRuntimeConfigName {
			return nil
		}
	}
	if err := client.IgnoreNotFound(c.Delete(ctx, legacy)); err != nil {
		return fmt.Errorf("delete the unused runtime config %s: %w", LegacyRuntimeConfigName, err)
	}
	out.Deleted = append(out.Deleted, "DeploymentRuntimeConfig/"+LegacyRuntimeConfigName)
	return nil
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
