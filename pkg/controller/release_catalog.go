package controller

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/release"
)

// pinValueProblems reports the component pins of a release whose values
// are unusable: not an object, or naming an unknown placeholder.
func pinValueProblems(content cloudv1alpha1.ReleaseContent) []string {
	var problems []string
	for _, k := range content.Kubernetes {
		for _, c := range k.Components {
			if _, err := component.PinValues(c.Values); err != nil {
				problems = append(problems, fmt.Sprintf("on %s, the values of %s: %v", k.Minor, c.Name, err))
			}
		}
	}
	return problems
}

// releasePins is a release's component pins as pkg/release takes them.
func releasePins(name string, content cloudv1alpha1.ReleaseContent) release.Release {
	return platform.ReleaseOf(name, content)
}

// CatalogEntries are the catalog's definitions as the release checks take
// them.
func CatalogEntries(defs []cloudv1alpha1.ComponentDefinition) []release.Definition {
	out := make([]release.Definition, 0, len(defs))
	for _, d := range defs {
		out = append(out, release.Definition{Name: d.Name, Requires: d.Spec.Requires,
			VersionGroup: d.Spec.VersionGroup, OneMinorAtATime: d.Spec.UpgradeOneMinorAtATime, Includes: d.Spec.Includes})
	}
	return out
}

// listCatalog reads the component catalog.
func listCatalog(ctx context.Context, c client.Client) ([]release.Definition, error) {
	list := &cloudv1alpha1.ComponentDefinitionList{}
	if err := c.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list component definitions: %w", err)
	}
	return CatalogEntries(list.Items), nil
}

// stepProblem says why a channel may not move from the release it offers
// (current) to next: a pin of next skips a minor of a component that
// upgrades one minor at a time. "" when it may, or when either release
// cannot be read (unusable reports that), or when current is not valid.
func stepProblem(ctx context.Context, c client.Client, current, next string) (string, error) {
	if current == "" || current == next {
		return "", nil
	}
	from, to := &cloudv1alpha1.Release{}, &cloudv1alpha1.Release{}
	if err := c.Get(ctx, client.ObjectKey{Name: current}, from); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: next}, to); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if meta.IsStatusConditionFalse(from.Status.Conditions, cloudv1alpha1.ReleaseConditionValid) {
		// Nothing can be installed from a release that is not valid (any
		// more): it is not a version to step from, and the channel must be
		// able to leave it.
		return "", nil
	}
	catalog, err := listCatalog(ctx, c)
	if err != nil {
		return "", err
	}
	problems := release.CheckStep(releasePins(current, from.Spec.Content), releasePins(next, to.Spec.Content), catalog)
	if len(problems) == 0 {
		return "", nil
	}
	return fmt.Sprintf("release %s cannot follow %s: %s", next, current, strings.Join(problems, "; ")), nil
}
