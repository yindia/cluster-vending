package management

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// MoveAdmitted implements reconcile.MoveAdmitter: the channel's release
// rollout (status.releaseMoves, which the rollout controller plans) lists
// the cluster among those that may start moving to release.
func (p platformSource) MoveAdmitted(ctx context.Context, channel, release, tenantID, cluster string) (bool, string, error) {
	ch := &cloudv1alpha1.ReleaseChannel{}
	if err := p.c.Get(ctx, client.ObjectKey{Name: channel}, ch); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("release channel %s does not exist", channel), nil
		}
		return false, "", err
	}
	return admittedBy(ch.Status.ReleaseMoves, release, kube.TenantNamespace(tenantID)+"/"+cluster)
}

// admittedBy reads a channel's release rollout for the cluster key
// (namespace/name).
func admittedBy(m *cloudv1alpha1.ChannelReleaseMoves, release, key string) (bool, string, error) {
	switch {
	case m == nil || m.Release != release:
		return false, fmt.Sprintf("waiting for the channel's rollout of release %s to be planned", release), nil
	case slices.Contains(m.Admitted, key):
		return true, "", nil
	case m.Halted:
		return false, "the channel's rollout of release " + release + " is halted: " + m.Waiting, nil
	case m.Waiting != "":
		return false, "waiting its turn in the channel's rollout: " + m.Waiting, nil
	}
	return false, "waiting its turn in the channel's rollout", nil
}
