package cluster

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// pinnedAddons are the add-ons the client gave a version (input: the
// cluster as sent); the others follow the version EKS recommends.
func pinnedAddons(input *cloudv1.ClusterSpec) map[string]string {
	out := map[string]string{}
	for _, a := range input.GetAws().GetAddons() {
		if a.GetVersion() != "" {
			out[a.GetName()] = a.GetVersion()
		}
	}
	return out
}

// resolveVersions sets spec's Kubernetes version (when empty: the EKS
// default) and every add-on's version for it: pinned ones are checked,
// the others become the version EKS recommends. Without an AWS lookup
// (tests) versions stay empty and EKS chooses at create.
func (s *ClusterService) resolveVersions(ctx context.Context, a *model.CloudAccount, spec *cloudv1.ClusterSpec,
	pinned map[string]string,
) error {
	if s.deps.AWSClusters == nil {
		return nil
	}
	access, err := s.deps.AWSAccess(ctx, a)
	if err != nil {
		return err
	}
	req := awscloud.VersionRequest{KubernetesVersion: spec.GetControlPlane().GetKubernetesVersion()}
	for _, addon := range spec.GetAws().GetAddons() {
		req.Addons = append(req.Addons, awscloud.AddonVersionRequest{Name: addon.GetName(), Version: pinned[addon.GetName()]})
	}
	got, err := s.deps.AWSClusters.ResolveEKSVersions(ctx, access, spec.GetRegion(), req)
	switch {
	case errors.Is(err, awscloud.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	case errors.Is(err, awscloud.ErrAccessDenied):
		return connect.NewError(connect.CodeFailedPrecondition, "cannot look up EKS versions with the account's credentials: "+err.Error())
	case err != nil:
		s.logger().ErrorContext(ctx, "resolve eks versions", "region", spec.GetRegion(), "error", err)
		return connect.NewError(connect.CodeUnavailable, "the AWS lookup of EKS versions failed: "+err.Error())
	}
	spec.ControlPlane.KubernetesVersion = got.KubernetesVersion
	for _, addon := range spec.GetAws().GetAddons() {
		addon.Version = got.Addons[addon.GetName()].Version
	}
	return nil
}

// reresolveVersions resolves spec's versions after an update of the
// Kubernetes version or the add-ons: the add-ons are merged into the
// defaults again, those the client did not pin follow the (new)
// Kubernetes version. A cluster stored without a Kubernetes version keeps
// the one it runs, so an add-on change does not upgrade it.
func (s *ClusterService) reresolveVersions(ctx context.Context, row *model.Cluster, a *model.CloudAccount,
	spec *cloudv1.ClusterSpec,
) error {
	input := &cloudv1.Cluster{}
	if len(row.Input) > 0 {
		if err := lenient.Unmarshal(row.Input, input); err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Sprintf("cluster %s: stored input: %v", row.ID, err))
		}
	}
	spec.GetAws().Addons = mergeAddons(spec.GetAws().GetAddons())
	if spec.GetControlPlane().GetKubernetesVersion() == "" {
		live, err := s.liveKubernetesVersion(ctx, row)
		if err != nil {
			return err
		}
		spec.ControlPlane.KubernetesVersion = live
	}
	return s.resolveVersions(ctx, a, spec, pinnedAddons(input.GetSpec()))
}

// liveKubernetesVersion is the version the EKS cluster reports; empty
// before EKS created it.
func (s *ClusterService) liveKubernetesVersion(ctx context.Context, row *model.Cluster) (string, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return "", service.ToConnect(err)
	}
	for _, r := range records {
		if r.Kind != "Cluster" {
			continue
		}
		c := &awseks.Cluster{}
		if err := s.getIgnoreMissing(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, c); err != nil {
			return "", connect.NewError(connect.CodeUnavailable, "cannot read the EKS cluster: "+err.Error())
		}
		return deref(c.Status.AtProvider.Version), nil
	}
	return "", nil
}
