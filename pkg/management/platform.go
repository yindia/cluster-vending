package management

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// PlatformSource is reconcile.PlatformSource on the management cluster:
// the component catalog (ComponentDefinitions), the release channels and
// the releases, read with c.
func PlatformSource(c client.Reader) reconcile.PlatformSource { return platformSource{c: c} }

type platformSource struct{ c client.Reader }

func (p platformSource) Definitions(ctx context.Context) ([]cloudv1alpha1.ComponentDefinition, error) {
	list := &cloudv1alpha1.ComponentDefinitionList{}
	if err := p.c.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list component definitions: %w", err)
	}
	return list.Items, nil
}

func (p platformSource) Offered(ctx context.Context, channel string) (string, error) {
	ch := &cloudv1alpha1.ReleaseChannel{}
	if err := p.c.Get(ctx, client.ObjectKey{Name: channel}, ch); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return ch.Status.Release, nil
}

func (p platformSource) Release(ctx context.Context, name string) (*cloudv1alpha1.Release, error) {
	rel := &cloudv1alpha1.Release{}
	if err := p.c.Get(ctx, client.ObjectKey{Name: name}, rel); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return rel, nil
}
