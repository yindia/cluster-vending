package cluster

import (
	"encoding/json"
	"fmt"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// noPlatformKey is the key, in the stored adoption of an imported cluster
// (a JSON object), under which its takeover recorded that the platform
// components are kept off (TakeOverClusterRequest.skip_platform_components).
const noPlatformKey = "skipPlatformComponents"

// oidcProviderKind is the kind of a cluster's IAM OIDC provider.
const oidcProviderKind = "OpenIDConnectProvider"

// WithoutPlatformComponents is adoption (the stored JSON object, or empty)
// with the mark that keeps the platform components off the cluster.
// TakeOverCluster stores it when the takeover asks for it: the cluster may
// already run its own load balancer controller, autoscaler or mesh.
func WithoutPlatformComponents(adoption model.JSON) (model.JSON, error) {
	fields := map[string]json.RawMessage{}
	if len(adoption) > 0 {
		if err := json.Unmarshal(adoption, &fields); err != nil {
			return nil, fmt.Errorf("stored adoption: %w", err)
		}
	}
	fields[noPlatformKey] = json.RawMessage("true")
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return model.JSON(out), nil
}

// PlatformKeptOff reports whether the cluster's takeover asked to keep the
// platform components off.
func PlatformKeptOff(row *model.Cluster) bool {
	if len(row.Adoption) == 0 {
		return false
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(row.Adoption, &fields); err != nil {
		return false
	}
	return string(fields[noPlatformKey]) == "true"
}

// noPlatform reports whether the platform components are not for this
// cluster: an imported cluster that was not taken over is only observed,
// nothing is installed on it; and a taken-over one whose takeover asked
// to keep them off. A taken-over cluster otherwise gets them like any
// cluster.
func noPlatform(row *model.Cluster) bool {
	if !row.Imported {
		return false
	}
	return row.State == StateImported || row.State == StateBlocked || PlatformKeptOff(row)
}

// PlatformManaged reports whether the platform manages the cluster's node
// groups: the system group, and the user groups' Karpenter objects. False
// for an imported cluster that is only observed (or whose takeover kept the
// platform components off).
func PlatformManaged(row *model.Cluster) bool { return !noPlatform(row) }

// HasIRSA reports whether a cluster with this spec and these recorded
// objects has IRSA: spec.aws.irsa, or an IAM OIDC provider among its
// objects (hasOIDCProvider).
func HasIRSA(spec *cloudv1.ClusterSpec, records []model.ManagedResource) bool {
	return spec.GetAws().GetIrsa() || hasOIDCProvider(records)
}

// hasOIDCProvider reports whether the cluster has an IAM OIDC provider
// among its objects: the one spec.aws.irsa renders, or the one it was
// imported with (carried over, while spec.aws.irsa stays off: it cannot
// be turned on over a carried-over provider). Either way the cluster has
// IRSA: a role can trust the provider.
func hasOIDCProvider(records []model.ManagedResource) bool {
	for _, r := range records {
		if r.Kind == oidcProviderKind {
			return true
		}
	}
	return false
}

// explainTakenOver adds, to the message of every failed component of a
// taken-over cluster, what is special about such a cluster: it may
// already run the component itself, which makes the platform's install
// fail (a Helm release or objects of the same name exist). The platform
// never deletes or adopts what the cluster came with; the provider's own
// error stays in front.
func explainTakenOver(row *model.Cluster, components []*cloudv1.PlatformComponent) {
	if !row.Imported {
		return
	}
	for _, c := range components {
		if c.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED {
			continue
		}
		c.Message += fmt.Sprintf("; cluster %s was taken over and may already run %s itself: the platform does not touch that one. "+
			"Uninstall the cluster's own %s and the platform's is installed on the next try, or keep the platform components "+
			"off such a cluster by taking it over with skip_platform_components", row.Name, c.GetName(), c.GetName())
	}
}
