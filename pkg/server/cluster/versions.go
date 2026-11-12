package cluster

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// prepareVersions gets spec ready for the reconciler to resolve its
// versions after an update of the Kubernetes version or the add-ons: the
// add-ons are merged into the defaults again, and a cluster stored
// without a Kubernetes version keeps the one it runs, so an add-on change
// does not upgrade it. It does not look at the cloud: which versions EKS
// offers is found out by the reconciler before it applies the change
// (reconcilecluster: resolve).
func (s *ClusterService) prepareVersions(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec) error {
	spec.GetAws().Addons = corecluster.MergeAddons(spec.GetAws().GetAddons())
	if spec.GetControlPlane().GetKubernetesVersion() != "" {
		return nil
	}
	live, err := s.LiveKubernetesVersion(ctx, row)
	if err != nil {
		return err
	}
	spec.ControlPlane.KubernetesVersion = live
	return nil
}

// withUnknownVersions is spec as a plan can show it before the reconciler
// resolved its versions: an add-on the client did not pin reads
// crossplaneaws.KnownAfterApply where its version is not known yet (the
// Kubernetes version changes, so EKS recommends another; or the add-on is
// new). An add-on that stays on the same Kubernetes version shows the
// version it has. spec is not changed.
func withUnknownVersions(row *model.Cluster, spec *cloudv1.ClusterSpec, versionBefore string) (*cloudv1.ClusterSpec, error) {
	input := &cloudv1.Cluster{}
	if len(row.Input) > 0 {
		if err := reconcilecluster.Lenient.Unmarshal(row.Input, input); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Sprintf("cluster %s: stored input: %v", row.ID, err))
		}
	}
	pinned := reconcilecluster.PinnedAddons(input.GetSpec())
	out := proto.CloneOf(spec)
	moved := out.GetControlPlane().GetKubernetesVersion() != versionBefore
	for _, addon := range out.GetAws().GetAddons() {
		switch version, ok := pinned[addon.GetName()]; {
		case ok:
			addon.Version = version
		case moved || addon.GetVersion() == "":
			addon.Version = crossplaneaws.KnownAfterApply
		}
	}
	return out, nil
}
