package cluster

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// unresolved reports whether row's generation still needs its cloud
// lookups before anything is rendered for it: the API stored it (a new
// cluster, or an update of its Kubernetes version or add-ons) without
// looking at the cloud. A generation that is applied needs none.
func unresolved(row *model.Cluster) bool {
	return row.ResolvedGeneration < row.Generation && row.AppliedGeneration < row.Generation
}

// PinnedAddons are the add-ons the client gave a version (input: the
// cluster as sent); the others follow the version EKS recommends.
func PinnedAddons(input *cloudv1.ClusterSpec) map[string]string {
	out := map[string]string{}
	for _, a := range input.GetAws().GetAddons() {
		if a.GetVersion() != "" {
			out[a.GetName()] = a.GetVersion()
		}
	}
	return out
}

// resolve makes the cloud lookups of row's generation and stores what
// they found, before anything is rendered:
//
//   - for a cluster nothing was created for yet, that EKS has no cluster
//     of the name in the account and region. The name is the EKS
//     cluster's; a taken one would make Crossplane adopt (and later
//     delete) a cluster made elsewhere;
//   - the Kubernetes version (when the spec has none: the EKS default)
//     and every add-on's version for it: the ones the client pinned are
//     checked, the others become the version EKS recommends. They are
//     written to the stored spec, so later renders do not depend on what
//     AWS answers then. The cluster as sent (input) is not changed.
//
// What EKS refuses (a taken name, a version it does not offer, an add-on
// version that does not fit) is a fault: the cluster is FAILED with the
// reason and nothing is created. A lookup that failed is tried again.
// Without an AWS lookup (tests) nothing is looked up and EKS chooses at
// create. It reports whether the row was stored; a row a call changed
// meanwhile is left for the next pass.
func (s *Reconciler) resolve(ctx context.Context, row *model.Cluster) (bool, error) {
	spec, err := SpecOf(row)
	if err != nil {
		return false, err
	}
	if s.deps.AWSClusters != nil {
		t, err := s.Target(ctx, row.TenantID, spec)
		if err != nil {
			return false, err
		}
		if err := s.checkEKSName(ctx, row, t.Account); err != nil {
			return false, err
		}
		pinned, err := pinnedOf(row)
		if err != nil {
			return false, err
		}
		if err := s.resolveVersions(ctx, t.Account, spec, pinned); err != nil {
			return false, err
		}
	}
	raw, err := Encode(spec)
	if err != nil {
		return false, fault.New(fault.Internal, err.Error())
	}
	before, resolved := row.Spec, row.ResolvedGeneration
	row.Spec, row.ResolvedGeneration = raw, row.Generation
	if err := s.deps.Store.Clusters().Record(ctx, row); err != nil {
		row.Spec, row.ResolvedGeneration = before, resolved
		if errors.Is(err, repository.ErrConflict) || errors.Is(err, repository.ErrNotFound) {
			return false, nil
		}
		return false, storeError(err)
	}
	return true, nil
}

// pinnedOf are the add-on versions the client pinned, from the cluster as
// it was sent.
func pinnedOf(row *model.Cluster) (map[string]string, error) {
	input := &cloudv1.Cluster{}
	if len(row.Input) > 0 {
		if err := Lenient.Unmarshal(row.Input, input); err != nil {
			return nil, fault.New(fault.Internal, fmt.Sprintf("cluster %s: stored input: %v", row.ID, err))
		}
	}
	return PinnedAddons(input.GetSpec()), nil
}

// EditedInput is the stored input of a cluster whose object's spec was edited
// (row.Spec is the edited spec): what an update through the API does with
// the add-ons it is given (UpdateCluster, spec.aws.addons), for an edit that
// has no request. The add-ons become the edited ones, and a version is a pin
// only when the client chose it: the version of an add-on that was pinned
// before (it is the one in the edit now), or one that is not what the add-on
// has installed (the resolved versions the object shows are not pins). A
// cleared version is no pin: the version EKS recommends is looked up again.
// installed maps add-on names to the versions the cluster has now.
func EditedInput(row *model.Cluster, installed map[string]string) (model.JSON, error) {
	if len(row.Input) == 0 {
		return row.Input, nil
	}
	input := &cloudv1.Cluster{}
	if err := Lenient.Unmarshal(row.Input, input); err != nil {
		return nil, fmt.Errorf("cluster %s: stored input: %w", row.ID, err)
	}
	edited, err := SpecOf(row)
	if err != nil {
		return nil, err
	}
	before := PinnedAddons(input.GetSpec())
	addons := make([]*cloudv1.AwsAddon, 0, len(edited.GetAws().GetAddons()))
	for _, a := range edited.GetAws().GetAddons() {
		in := proto.Clone(a).(*cloudv1.AwsAddon)
		if _, was := before[a.GetName()]; !was && a.GetVersion() == installed[a.GetName()] {
			in.Version = ""
		}
		addons = append(addons, in)
	}
	if input.Spec == nil {
		input.Spec = &cloudv1.ClusterSpec{}
	}
	if input.Spec.GetAws() == nil {
		input.Spec.Provider = &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}
	}
	input.Spec.GetAws().Addons = addons
	return Encode(input)
}

// checkEKSName refuses a cluster name EKS already has in the account and
// region, for a cluster the platform created nothing for yet (once it
// did, the EKS cluster of the name is its own).
func (s *Reconciler) checkEKSName(ctx context.Context, row *model.Cluster, a *model.CloudAccount) error {
	if row.ResolvedGeneration != 0 || row.Imported {
		return nil
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return storeError(err)
	}
	if len(records) > 0 {
		return nil
	}
	access, err := s.deps.AWSAccess(ctx, a)
	if err != nil {
		return err
	}
	exists, err := s.deps.AWSClusters.EKSClusterExists(ctx, access, row.Region, row.Name)
	switch {
	case errors.Is(err, awscloud.ErrAccessDenied):
		return fault.New(fault.FailedPrecondition, "cannot check the EKS cluster name with the account's credentials: "+err.Error())
	case err != nil:
		s.logger().ErrorContext(ctx, "check eks cluster name", "name", row.Name, "region", row.Region, "error", err)
		return fault.New(fault.Unavailable, "the AWS lookup of the EKS cluster name failed: "+err.Error())
	case exists:
		return fault.New(fault.AlreadyExists, fmt.Sprintf(
			"account %s already has an EKS cluster named %q in %s (not created by the platform); choose another name",
			a.AccountID, row.Name, row.Region))
	}
	return nil
}

// resolveVersions sets spec's Kubernetes version (when empty: the EKS
// default) and every add-on's version for it: pinned ones are checked,
// the others become the version EKS recommends.
func (s *Reconciler) resolveVersions(ctx context.Context, a *model.CloudAccount, spec *cloudv1.ClusterSpec, pinned map[string]string) error {
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
		return fault.New(fault.Invalid, err.Error())
	case errors.Is(err, awscloud.ErrAccessDenied):
		return fault.New(fault.FailedPrecondition, "cannot look up EKS versions with the account's credentials: "+err.Error())
	case err != nil:
		s.logger().ErrorContext(ctx, "resolve eks versions", "region", spec.GetRegion(), "error", err)
		return fault.New(fault.Unavailable, "the AWS lookup of EKS versions failed: "+err.Error())
	}
	if spec.ControlPlane == nil {
		spec.ControlPlane = &cloudv1.ControlPlaneSpec{}
	}
	spec.ControlPlane.KubernetesVersion = got.KubernetesVersion
	for _, addon := range spec.GetAws().GetAddons() {
		addon.Version = got.Addons[addon.GetName()].Version
	}
	return nil
}
