package cluster

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
	"github.com/yindia/cluster-vending/pkg/updatemask"
)

// Update mask paths with no cloud effect.
const (
	pathDisplayName = "display_name"
	pathLabels      = "labels"
)

// Spec paths that make the platform resolve versions again.
const (
	pathKubernetesVersion = "spec.control_plane.kubernetes_version"
	pathAddons            = "spec.aws.addons"
	pathSecretsEncryption = "spec.control_plane.secrets_encryption"
)

// pathNodeGroups is the user's Karpenter node groups, one path for the whole
// list.
const pathNodeGroups = "spec.node_groups"

// managedNodeGroupPaths are the default node group's fields that the platform
// owns: an update that names one is refused. Instance types, capacity type
// and disk size stay changeable.
var managedNodeGroupPaths = []string{
	"spec.default_node_pool.taints",
	"spec.default_node_pool.labels",
	"spec.default_node_pool.scaling_config",
}

// errDefaultNodeGroupManaged is the refusal of a change to a field of the
// default node group the platform owns.
func errDefaultNodeGroupManaged(path string) error {
	return connect.NewError(connect.CodeFailedPrecondition,
		fmt.Sprintf("the default node group is managed by the platform: %s cannot be changed", path))
}

// isManagedNodeGroupPath reports whether path is, or is inside, one of
// managedNodeGroupPaths.
func isManagedNodeGroupPath(path string) bool {
	return slices.ContainsFunc(managedNodeGroupPaths, func(m string) bool {
		return path == m || strings.HasPrefix(path, m+".")
	})
}

// Spec paths that only say how platform releases treat the cluster: no
// rendered object depends on them, so no cloud resource changes.
const (
	pathUpgradeMode    = "spec.upgrade_mode"
	pathReleaseChannel = "spec.release_channel"
	// When an automatic upgrade may start: read by the rollout, never
	// rendered.
	pathMaintenanceWindow = "spec.maintenance_window"
)

// releasePaths are the paths of releaseOnly updates.
var releasePaths = []string{pathUpgradeMode, pathReleaseChannel, pathMaintenanceWindow}

// releaseOnly reports whether every path of an update is one of
// releasePaths: the update changes no cloud resource.
func releaseOnly(paths []string) bool {
	for _, p := range paths {
		if !slices.Contains(releasePaths, p) {
			return false
		}
	}
	return len(paths) > 0
}

// specPaths are the spec paths UpdateCluster changes, each copying one
// field of the request's spec onto the stored one. Every one of them but
// releasePaths is rolled out by applying the cluster's resources again.
var specPaths = map[string]func(dst, src *cloudv1.ClusterSpec){
	"spec.naas": func(dst, src *cloudv1.ClusterSpec) {
		if src == nil {
			dst.Naas = nil
		} else {
			dst.Naas = src.Naas
		}
	},
	// The user's node groups are compared and applied as a whole list.
	pathNodeGroups: func(dst, src *cloudv1.ClusterSpec) {
		dst.NodeGroups = proto.CloneOf(&cloudv1.ClusterSpec{NodeGroups: src.GetNodeGroups()}).GetNodeGroups()
	},
	pathUpgradeMode:    func(dst, src *cloudv1.ClusterSpec) { dst.UpgradeMode = src.GetUpgradeMode() },
	pathReleaseChannel: func(dst, src *cloudv1.ClusterSpec) { dst.ReleaseChannel = src.GetReleaseChannel() },
	pathMaintenanceWindow: func(dst, src *cloudv1.ClusterSpec) {
		dst.MaintenanceWindow = src.GetMaintenanceWindow()
	},
	pathKubernetesVersion: func(dst, src *cloudv1.ClusterSpec) {
		dst.ControlPlane.KubernetesVersion = src.GetControlPlane().GetKubernetesVersion()
	},
	"spec.control_plane.endpoint_access.public_access_cidrs": func(dst, src *cloudv1.ClusterSpec) {
		dst.ControlPlane.EndpointAccess.PublicAccessCidrs = src.GetControlPlane().GetEndpointAccess().GetPublicAccessCidrs()
	},
	// EKS cannot change these in place: the node group is replaced.
	"spec.default_node_pool.instance_types": func(dst, src *cloudv1.ClusterSpec) {
		dst.DefaultNodePool.InstanceTypes = src.GetDefaultNodePool().GetInstanceTypes()
	},
	"spec.default_node_pool.capacity_type": func(dst, src *cloudv1.ClusterSpec) {
		dst.DefaultNodePool.CapacityType = src.GetDefaultNodePool().GetCapacityType()
	},
	"spec.default_node_pool.disk_size_gb": func(dst, src *cloudv1.ClusterSpec) {
		dst.DefaultNodePool.DiskSizeGb = src.GetDefaultNodePool().GetDiskSizeGb()
	},
	"spec.aws.enabled_cluster_log_types": func(dst, src *cloudv1.ClusterSpec) {
		dst.GetAws().EnabledClusterLogTypes = src.GetAws().GetEnabledClusterLogTypes()
	},
	"spec.aws.upgrade_policy.support_type": func(dst, src *cloudv1.ClusterSpec) {
		if dst.GetAws().UpgradePolicy == nil {
			dst.GetAws().UpgradePolicy = &cloudv1.AwsUpgradePolicy{}
		}
		dst.GetAws().UpgradePolicy.SupportType = src.GetAws().GetUpgradePolicy().GetSupportType()
	},
	pathAddons: func(dst, src *cloudv1.ClusterSpec) {
		dst.GetAws().Addons = src.GetAws().GetAddons()
	},
	"spec.access.admin_principals": func(dst, src *cloudv1.ClusterSpec) {
		if dst.Access == nil {
			dst.Access = &cloudv1.AccessSpec{}
		}
		dst.Access.AdminPrincipals = src.GetAccess().GetAdminPrincipals()
	},
	// Entries and their policies are objects of their own: added, removed
	// and replaced without touching the cluster.
	"spec.access.access_entries": func(dst, src *cloudv1.ClusterSpec) {
		if dst.Access == nil {
			dst.Access = &cloudv1.AccessSpec{}
		}
		dst.Access.AccessEntries = src.GetAccess().GetAccessEntries()
	},
	// The identity provider is an object of its own too. EKS cannot change
	// one: a change deletes it and creates the new one.
	"spec.access.oidc": func(dst, src *cloudv1.ClusterSpec) {
		if dst.Access == nil {
			dst.Access = &cloudv1.AccessSpec{}
		}
		dst.Access.Oidc = src.GetAccess().GetOidc()
	},
	"spec.control_plane.endpoint_access.endpoint_private_access": func(dst, src *cloudv1.ClusterSpec) {
		endpointAccess(dst).EndpointPrivateAccess = optionalBool(src.GetControlPlane().GetEndpointAccess(), func(a *cloudv1.EndpointAccess) *bool { return a.EndpointPrivateAccess })
	},
	"spec.control_plane.endpoint_access.endpoint_public_access": func(dst, src *cloudv1.ClusterSpec) {
		endpointAccess(dst).EndpointPublicAccess = optionalBool(src.GetControlPlane().GetEndpointAccess(), func(a *cloudv1.EndpointAccess) *bool { return a.EndpointPublicAccess })
	},
	// Can be turned on, never off (UpdateCluster checks: corecluster.EncryptionKept).
	pathSecretsEncryption: func(dst, src *cloudv1.ClusterSpec) {
		dst.ControlPlane.SecretsEncryption = src.GetControlPlane().GetSecretsEncryption()
	},
	// Turned on, the OIDC provider is created; turned off, deleted.
	"spec.aws.irsa": func(dst, src *cloudv1.ClusterSpec) {
		dst.GetAws().Irsa = optionalBool(src.GetAws(), func(a *cloudv1.AwsClusterSpec) *bool { return a.Irsa })
	},
	"spec.aws.control_plane_log_group": func(dst, src *cloudv1.ClusterSpec) {
		dst.GetAws().ControlPlaneLogGroup = src.GetAws().GetControlPlaneLogGroup()
	},
	// EKS cannot swap a node group's launch template or AMI type in place:
	// the node group is replaced. A new version of the same template is
	// rolled out in place.
	"spec.aws.default_node_pool.launch_template": func(dst, src *cloudv1.ClusterSpec) {
		awsNodePool(dst).LaunchTemplate = src.GetAws().GetDefaultNodePool().GetLaunchTemplate()
	},
	"spec.aws.default_node_pool.ami_type": func(dst, src *cloudv1.ClusterSpec) {
		awsNodePool(dst).AmiType = src.GetAws().GetDefaultNodePool().GetAmiType()
	},
}

// optionalBool reads an optional bool of a message the request may have
// left out altogether: then it is unset, not a nil dereference.
func optionalBool[M any](m *M, field func(*M) *bool) *bool {
	if m == nil {
		return nil
	}
	return field(m)
}

func endpointAccess(spec *cloudv1.ClusterSpec) *cloudv1.EndpointAccess {
	if spec.ControlPlane == nil {
		spec.ControlPlane = &cloudv1.ControlPlaneSpec{}
	}
	if spec.ControlPlane.EndpointAccess == nil {
		spec.ControlPlane.EndpointAccess = &cloudv1.EndpointAccess{}
	}
	return spec.ControlPlane.EndpointAccess
}

func awsNodePool(spec *cloudv1.ClusterSpec) *cloudv1.AwsNodePoolSpec {
	if spec.GetAws().DefaultNodePool == nil {
		spec.GetAws().DefaultNodePool = &cloudv1.AwsNodePoolSpec{}
	}
	return spec.GetAws().DefaultNodePool
}

// UpdateCluster changes the fields named in update_mask in the stored
// cluster. The reconciler rolls spec changes out by applying the cluster's
// resources again; resources the new spec no longer has (a removed admin's
// access entry) are deleted. It does not look at the cloud: after a change
// of the Kubernetes version or the add-ons the reconciler resolves the
// versions against what EKS offers first, and a version EKS refuses is
// the cluster's status (FAILED), not the answer of this call. With
// validate_only it only returns the plan of the change (planUpdate, with
// versions EKS has yet to choose shown as known after apply); with
// plan_id it stores the change only if the plan is still the same.
func (s *ClusterService) UpdateCluster(ctx context.Context, req *cloudv1.UpdateClusterRequest) (_ *cloudv1.UpdateClusterResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	//nolint:staticcheck // Reject legacy per-cluster tier writes with migration guidance.
	if req.GetCluster().GetSpec().GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, "tier is inherited from the network; set spec.tier on the network")
	}
	in := req.GetCluster()
	row, err := s.get(ctx, req.GetTenantId(), in.GetId())
	if err != nil {
		return nil, err
	}
	if err := checkMutable(row, in.GetEtag()); err != nil {
		return nil, err
	}
	spec, err := reconcilecluster.SpecOf(row)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	// On an automatic cluster the channel sets the version: a manifest's is
	// not a request (the rollout moves it), so it is left out.
	paths, dropped := updatemask.WithoutAutomaticVersion(spec, in.GetSpec(), req.GetUpdateMask().GetPaths())
	if dropped && len(paths) == 0 {
		// Only the version was asked for: nothing changes.
		c, err := s.stored(ctx, row)
		return &cloudv1.UpdateClusterResponse{Cluster: c}, err
	}
	versionBefore, encryptionBefore := spec.GetControlPlane().GetKubernetesVersion(), corecluster.Encryption(spec)
	nodeGroupsBefore := spec.GetNodeGroups()
	specChanged, err := applyMask(row, spec, in, paths)
	if err != nil {
		return nil, err
	}
	if slices.Contains(paths, pathNodeGroups) {
		if err := checkNodeGroupAllowlist(spec.GetNodeGroups(), nodeGroupsBefore,
			s.deps.NodeGroupInstanceFamilies, s.deps.NodeGroupInstanceTypes); err != nil {
			return nil, err
		}
	}
	if slices.Contains(paths, "spec.naas") && !spec.GetNaas() {
		if err := s.requireNoSubTenants(ctx, row); err != nil {
			return nil, err
		}
	}
	if err := s.CarriedOverConflict(ctx, row, spec); err != nil {
		return nil, err
	}
	// A plan shows the refusal (REJECTED_IMMUTABLE); an update fails.
	if !corecluster.EncryptionKept(encryptionBefore, corecluster.Encryption(spec)) && !req.GetValidateOnly() {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			"secrets encryption cannot be turned off or moved to another key once it is on: EKS cannot change it")
	}
	if slices.Contains(paths, pathKubernetesVersion) {
		if err := s.CheckVersionStep(ctx, row, versionBefore, spec.GetControlPlane().GetKubernetesVersion()); err != nil {
			return nil, err
		}
		if err := s.CheckChannelVersion(ctx, spec, versionBefore); err != nil {
			return nil, err
		}
	}
	if row.Input, err = updatedInput(row, in, paths); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	t, err := s.Target(ctx, row.TenantID, spec)
	if err != nil {
		return nil, err
	}
	// A change of the Kubernetes version or the add-ons has versions to
	// look up at EKS: the reconciler does that before it applies the
	// change (the row's resolved generation stays behind).
	unresolved := slices.Contains(paths, pathKubernetesVersion) || slices.Contains(paths, pathAddons)
	if unresolved {
		if err := s.prepareVersions(ctx, row, spec); err != nil {
			return nil, err
		}
		specChanged = true
	}
	var plan *cloudv1.Plan
	if req.GetValidateOnly() || req.GetPlanId() != "" {
		shown := spec
		if unresolved {
			// The versions EKS will choose are not known here.
			if shown, err = withUnknownVersions(row, spec, versionBefore); err != nil {
				return nil, err
			}
		}
		if plan, err = s.planUpdate(ctx, req, row, shown, t); err != nil {
			return nil, err
		}
		if req.GetValidateOnly() {
			return s.planned(ctx, req, plan)
		}
		if plan.GetPlanId() != req.GetPlanId() {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				"the plan is stale: the request or a resource changed since it was made; plan again (validate_only)")
		}
		if !plan.GetApplicable() {
			return nil, connect.NewError(connect.CodeFailedPrecondition, "the plan is not applicable: it changes a field the cloud cannot change")
		}
	}
	if row.Spec, err = reconcilecluster.Encode(spec); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	if specChanged {
		row.Generation++
		if !unresolved && row.ResolvedGeneration == row.Generation-1 {
			row.ResolvedGeneration = row.Generation // nothing to look up for this change
		}
		if releaseOnly(paths) {
			// Nothing of the cloud changes: the new generation counts as
			// applied as soon as the one before it was, and the cluster's
			// state stays what it is.
			if row.AppliedGeneration == row.Generation-1 {
				row.AppliedGeneration = row.Generation
			}
		} else {
			row.State = reconcilecluster.StateUpdating
			reconcilecluster.ClearFailure(row) // of the generation before: the next pass reports on this one
		}
	}
	if err := s.deps.Store.Clusters().Update(ctx, row); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, connect.NewError(connect.CodeAborted, "the cluster changed; get it again and retry with its etag")
		}
		return nil, service.ToConnect(err)
	}
	if specChanged {
		s.logger().InfoContext(ctx, "cluster update stored", "cluster_id", row.ID, "paths", paths)
	}
	c, err := s.stored(ctx, row)
	return &cloudv1.UpdateClusterResponse{Cluster: c, Plan: plan, UpdateRecorded: true}, err
}

// planned is a validate_only response: the plan, and the cluster as it is
// (nothing was stored or applied).
func (s *ClusterService) planned(ctx context.Context, req *cloudv1.UpdateClusterRequest, plan *cloudv1.Plan) (*cloudv1.UpdateClusterResponse, error) {
	row, err := s.get(ctx, req.GetTenantId(), req.GetCluster().GetId())
	if err != nil {
		return nil, err
	}
	c, err := s.View(ctx, row)
	return &cloudv1.UpdateClusterResponse{Cluster: c, Plan: plan}, err
}

// checkMutable refuses updates of deleting clusters and stale etags.
func checkMutable(row *model.Cluster, etag string) error {
	if row.State == reconcilecluster.StateDeleting {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("cluster %s is being deleted", row.ID))
	}
	if row.State == reconcilecluster.StateImported || row.State == reconcilecluster.StateBlocked {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("cluster %s is imported read-only; take it over first", row.ID))
	}
	if etag != "" && etag != row.ETag {
		return connect.NewError(connect.CodeAborted, "etag mismatch: the cluster changed; get it again and retry")
	}
	return nil
}

// updatablePaths are the paths UpdateCluster accepts (updatemask.Cluster,
// which the CLI infers masks from; a test keeps it equal to specPaths).
func updatablePaths() string { return strings.Join(updatemask.Cluster, ", ") }

// applyMask copies the masked fields of in onto row and spec and checks
// the result against the contract. It reports whether the cluster must be
// applied again: the spec changed, or the labels (the AWS tags) did.
func applyMask(row *model.Cluster, spec *cloudv1.ClusterSpec, in *cloudv1.Cluster, paths []string) (bool, error) {
	if len(paths) == 0 {
		return false, connect.NewError(connect.CodeInvalidArgument, "update_mask is empty; updatable: "+updatablePaths())
	}
	before, err := reconcilecluster.Encode(spec)
	if err != nil {
		return false, connect.NewError(connect.CodeInternal, err.Error())
	}
	labelsBefore := maps.Clone(row.Labels)
	for _, p := range paths {
		switch copyField, ok := specPaths[p]; {
		case p == pathDisplayName:
			row.DisplayName = in.GetDisplayName()
		case p == pathLabels:
			row.Labels = model.Labels(in.GetLabels())
		case isManagedNodeGroupPath(p):
			return false, errDefaultNodeGroupManaged(p)
		case ok:
			copyField(spec, in.GetSpec())
		case p == "spec.aws.access_config.authentication_mode":
			return false, connect.NewError(connect.CodeUnimplemented, fmt.Sprintf("updating %s is not implemented yet", p))
		default:
			return false, connect.NewError(connect.CodeInvalidArgument,
				fmt.Sprintf("update_mask path %q cannot be updated; updatable: %s", p, updatablePaths()))
		}
	}
	merged := &cloudv1.Cluster{Name: row.Name, DisplayName: row.DisplayName, Labels: row.Labels, Spec: spec}
	if err := protovalidate.Validate(merged); err != nil {
		return false, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := corecluster.ResolveOptions(spec); err != nil {
		return false, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	corecluster.ResolveReleaseFields(spec) // an unset mode or tier is stored as its default
	if err := service.CheckAWSLabels(row.Labels); err != nil {
		return false, err
	}
	after, err := reconcilecluster.Encode(spec)
	if err != nil {
		return false, connect.NewError(connect.CodeInternal, err.Error())
	}
	return string(before) != string(after) || !maps.Equal(labelsBefore, row.Labels), nil
}

// updatedInput is the stored input with the masked fields copied over, so
// it stays the cluster as the client asked for it (defaults unresolved).
func updatedInput(row *model.Cluster, in *cloudv1.Cluster, paths []string) (model.JSON, error) {
	if len(row.Input) == 0 {
		return row.Input, nil
	}
	input := &cloudv1.Cluster{}
	if err := reconcilecluster.Lenient.Unmarshal(row.Input, input); err != nil {
		return nil, fmt.Errorf("cluster %s: stored input: %w", row.ID, err)
	}
	for _, p := range paths {
		switch p {
		case pathDisplayName:
			input.DisplayName = in.GetDisplayName()
		case pathLabels:
			input.Labels = in.GetLabels()
		default:
			if copyField, ok := specPaths[p]; ok {
				ensureSpec(input)
				copyField(input.Spec, in.GetSpec())
			}
		}
	}
	return reconcilecluster.Encode(input)
}

// ensureSpec creates the messages the spec path setters write into.
func ensureSpec(c *cloudv1.Cluster) {
	if c.Spec == nil {
		c.Spec = &cloudv1.ClusterSpec{}
	}
	if c.Spec.ControlPlane == nil {
		c.Spec.ControlPlane = &cloudv1.ControlPlaneSpec{}
	}
	if c.Spec.ControlPlane.EndpointAccess == nil {
		c.Spec.ControlPlane.EndpointAccess = &cloudv1.EndpointAccess{}
	}
	if c.Spec.DefaultNodePool == nil {
		c.Spec.DefaultNodePool = &cloudv1.NodePoolSpec{}
	}
	if c.Spec.GetAws() == nil {
		c.Spec.Provider = &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}
	}
}

// DeleteCluster stores the cluster as DELETING. The reconciler deletes its
// resources and removes the cluster once they are gone.
func (s *ClusterService) DeleteCluster(ctx context.Context, req *cloudv1.DeleteClusterRequest) (_ *cloudv1.DeleteClusterResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	if etag := req.GetEtag(); etag != "" && etag != row.ETag {
		return nil, connect.NewError(connect.CodeAborted, "etag mismatch: the cluster changed; get it again and retry")
	}
	if err := s.requireNoSubTenants(ctx, row); err != nil {
		return nil, err
	}
	if row.State != reconcilecluster.StateDeleting || (req.GetForce() && !row.DeleteForce) {
		row.State = reconcilecluster.StateDeleting
		row.DeleteForce = row.DeleteForce || req.GetForce()
		reconcilecluster.ClearFailure(row)
		if err := s.deps.Store.Clusters().Update(ctx, row); err != nil {
			return nil, service.ToConnect(err)
		}
		s.logger().InfoContext(ctx, "cluster deleting", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name)
	}
	c, err := s.stored(ctx, row)
	return &cloudv1.DeleteClusterResponse{Cluster: c}, err
}
