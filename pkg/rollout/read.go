package rollout

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// Read reads what planning ch's rollout needs: the release
// it offers (nil when none or gone), every cluster and every release
// hold.
func Read(ctx context.Context, c client.Reader, ch *cloudv1alpha1.ReleaseChannel) (Input, error) {
	in := Input{Channel: ch}
	if ch.Status.Release != "" {
		rel := &cloudv1alpha1.Release{}
		switch err := c.Get(ctx, client.ObjectKey{Name: ch.Status.Release}, rel); {
		case err == nil:
			in.Release = rel
		case client.IgnoreNotFound(err) != nil:
			return in, fmt.Errorf("read release %s: %w", ch.Status.Release, err)
		}
	}
	clusters := &cloudv1alpha1.ClusterList{}
	if err := c.List(ctx, clusters); err != nil {
		return in, fmt.Errorf("list clusters: %w", err)
	}
	holds := &cloudv1alpha1.ReleaseHoldList{}
	if err := c.List(ctx, holds); err != nil {
		return in, fmt.Errorf("list release holds: %w", err)
	}
	in.Clusters, in.Holds = clusters.Items, holds.Items
	return in, nil
}
