package cluster

import (
	"fmt"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// SetAutomaticVersion changes row to the Kubernetes minor an automatic
// upgrade moves it to, as UpdateCluster of
// spec.control_plane.kubernetes_version does: the spec and the client's
// input ask for it, the add-ons are merged with the defaults (their
// versions are looked up again for the new minor), and a new generation
// is stored for the reconciler to resolve and apply (its resolved
// generation stays behind). The caller stores row, etag-guarded.
func SetAutomaticVersion(row *model.Cluster, version string) error {
	spec, err := SpecOf(row)
	if err != nil {
		return err
	}
	if spec.ControlPlane == nil {
		spec.ControlPlane = &cloudv1.ControlPlaneSpec{}
	}
	spec.ControlPlane.KubernetesVersion = version
	if aws := spec.GetAws(); aws != nil {
		aws.Addons = corecluster.MergeAddons(aws.GetAddons())
	}
	if row.Spec, err = Encode(spec); err != nil {
		return err
	}
	if len(row.Input) > 0 {
		input := &cloudv1.Cluster{}
		if err := Lenient.Unmarshal(row.Input, input); err != nil {
			return fmt.Errorf("cluster %s: stored input: %w", row.ID, err)
		}
		if input.Spec == nil {
			input.Spec = &cloudv1.ClusterSpec{}
		}
		if input.Spec.ControlPlane == nil {
			input.Spec.ControlPlane = &cloudv1.ControlPlaneSpec{}
		}
		input.Spec.ControlPlane.KubernetesVersion = version
		if row.Input, err = Encode(input); err != nil {
			return err
		}
	}
	row.Generation++
	row.State = StateUpdating
	ClearFailure(row)
	return nil
}
