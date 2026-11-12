// Package catalog installs the component catalog this build ships
// (config/catalog) into the management cluster: each ComponentDefinition
// that does not exist yet is created. An existing one is never changed:
// once the platform team has edited a definition, or published its own, a
// new build does not undo it.
package catalog

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	shipped "github.com/yindia/cluster-vending/config/catalog"
)

// Result names the definitions created and those left as they were.
type Result struct {
	Created, Existing []string
}

// Install creates the shipped definitions that are missing. The
// CustomResourceDefinition of ComponentDefinition must be served.
func Install(ctx context.Context, c client.Client) (Result, error) {
	defs, err := shipped.Definitions()
	if err != nil {
		return Result{}, fmt.Errorf("read the shipped catalog: %w", err)
	}
	return Create(ctx, c, defs)
}

// Create creates the definitions that do not exist and leaves the others
// untouched. The inputs are not changed.
func Create(ctx context.Context, c client.Client, defs []*cloudv1alpha1.ComponentDefinition) (Result, error) {
	var out Result
	for _, def := range defs {
		err := c.Create(ctx, def.DeepCopy())
		switch {
		case apierrors.IsAlreadyExists(err):
			out.Existing = append(out.Existing, def.Name)
		case err != nil:
			return out, fmt.Errorf("create component definition %s: %w", def.Name, err)
		default:
			out.Created = append(out.Created, def.Name)
		}
	}
	return out, nil
}
