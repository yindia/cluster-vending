package cluster

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
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
)

// specPaths are the spec paths UpdateCluster changes, each copying one
// field of the request's spec onto the stored one. Every one of them is
// rolled out by applying the cluster's resources again.
var specPaths = map[string]func(dst, src *cloudv1.ClusterSpec){
	"spec.tags": func(dst, src *cloudv1.ClusterSpec) { dst.Tags = src.GetTags() },
	pathKubernetesVersion: func(dst, src *cloudv1.ClusterSpec) {
		dst.ControlPlane.KubernetesVersion = src.GetControlPlane().GetKubernetesVersion()
	},
	"spec.control_plane.endpoint_access.public_access_cidrs": func(dst, src *cloudv1.ClusterSpec) {
		dst.ControlPlane.EndpointAccess.PublicAccessCidrs = src.GetControlPlane().GetEndpointAccess().GetPublicAccessCidrs()
	},
	"spec.default_node_pool.scaling_config.min_size": func(dst, src *cloudv1.ClusterSpec) {
		scaling(dst).MinSize = src.GetDefaultNodePool().GetScalingConfig().GetMinSize()
	},
	"spec.default_node_pool.scaling_config.max_size": func(dst, src *cloudv1.ClusterSpec) {
		scaling(dst).MaxSize = src.GetDefaultNodePool().GetScalingConfig().GetMaxSize()
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
	"spec.default_node_pool.labels": func(dst, src *cloudv1.ClusterSpec) {
		dst.DefaultNodePool.Labels = src.GetDefaultNodePool().GetLabels()
	},
	"spec.default_node_pool.taints": func(dst, src *cloudv1.ClusterSpec) {
		dst.DefaultNodePool.Taints = src.GetDefaultNodePool().GetTaints()
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
}

func scaling(spec *cloudv1.ClusterSpec) *cloudv1.ScalingConfig {
	if spec.DefaultNodePool.ScalingConfig == nil {
		spec.DefaultNodePool.ScalingConfig = &cloudv1.ScalingConfig{}
	}
	return spec.DefaultNodePool.ScalingConfig
}

// UpdateCluster changes the fields named in update_mask and rolls spec
// changes out by applying the cluster's resources again; resources the new
// spec no longer has (a removed admin's access entry) are deleted.
func (s *ClusterService) UpdateCluster(ctx context.Context, req *cloudv1.UpdateClusterRequest) (*cloudv1.UpdateClusterResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	in := req.GetCluster()
	row, err := s.get(ctx, req.GetTenantId(), in.GetId())
	if err != nil {
		return nil, err
	}
	if err := checkMutable(row, in.GetEtag()); err != nil {
		return nil, err
	}
	spec, err := specOf(row)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	paths := req.GetUpdateMask().GetPaths()
	specChanged, err := applyMask(row, spec, in, paths)
	if err != nil {
		return nil, err
	}
	if row.Input, err = updatedInput(row, in, paths); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	t, err := s.target(ctx, row.TenantID, spec)
	if err != nil {
		return nil, err
	}
	if slices.Contains(paths, pathKubernetesVersion) || slices.Contains(paths, pathAddons) {
		if err := s.reresolveVersions(ctx, row, t.account, spec); err != nil {
			return nil, err
		}
	}
	if row.Spec, err = encode(spec); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	if specChanged {
		row.Generation++
		row.State = stateUpdating
	}
	if err := s.deps.Store.Clusters().Update(ctx, row); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, connect.NewError(connect.CodeAborted, "the cluster changed; get it again and retry with its etag")
		}
		return nil, service.ToConnect(err)
	}
	if specChanged {
		plan := clusterPlan(spec, t.subnetIDs, s.base(row, t.account))
		if err := s.apply(ctx, row, t.account, plan); err != nil {
			return nil, err
		}
		s.logger().InfoContext(ctx, "cluster updated", "cluster_id", row.ID, "paths", paths)
	}
	// Idempotent: also tags the subnets of clusters created before the
	// platform tagged them.
	if err := s.tagSubnets(ctx, row, t); err != nil {
		return nil, err
	}
	c, err := s.view(ctx, row)
	return &cloudv1.UpdateClusterResponse{Cluster: c}, err
}

// checkMutable refuses updates of deleting clusters and stale etags.
func checkMutable(row *model.Cluster, etag string) error {
	if row.State == stateDeleting {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("cluster %s is being deleted", row.ID))
	}
	if etag != "" && etag != row.ETag {
		return connect.NewError(connect.CodeAborted, "etag mismatch: the cluster changed; get it again and retry")
	}
	return nil
}

func updatablePaths() string {
	paths := make([]string, 0, 2+len(specPaths))
	paths = append(paths, pathDisplayName, pathLabels)
	for p := range specPaths {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return strings.Join(paths, ", ")
}

// applyMask copies the masked fields of in onto row and spec and checks
// the result against the contract. It reports whether the spec changed.
func applyMask(row *model.Cluster, spec *cloudv1.ClusterSpec, in *cloudv1.Cluster, paths []string) (bool, error) {
	if len(paths) == 0 {
		return false, connect.NewError(connect.CodeInvalidArgument, "update_mask is empty; updatable: "+updatablePaths())
	}
	before, err := encode(spec)
	if err != nil {
		return false, connect.NewError(connect.CodeInternal, err.Error())
	}
	for _, p := range paths {
		switch copyField, ok := specPaths[p]; {
		case p == pathDisplayName:
			row.DisplayName = in.GetDisplayName()
		case p == pathLabels:
			row.Labels = model.Labels(in.GetLabels())
		case ok:
			copyField(spec, in.GetSpec())
		case strings.HasPrefix(p, "spec.existing_cluster"), p == "spec.control_plane.secrets_encryption.enabled",
			p == "spec.aws.access_config.authentication_mode":
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
	after, err := encode(spec)
	if err != nil {
		return false, connect.NewError(connect.CodeInternal, err.Error())
	}
	return string(before) != string(after), nil
}

// updatedInput is the stored input with the masked fields copied over, so
// it stays the cluster as the client asked for it (defaults unresolved).
func updatedInput(row *model.Cluster, in *cloudv1.Cluster, paths []string) (model.JSON, error) {
	if len(row.Input) == 0 {
		return row.Input, nil
	}
	input := &cloudv1.Cluster{}
	if err := protojson.Unmarshal(row.Input, input); err != nil {
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
	return encode(input)
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

// DeleteCluster deletes the cluster's resources; the cluster is removed
// once they are gone (until then it is DELETING).
func (s *ClusterService) DeleteCluster(ctx context.Context, req *cloudv1.DeleteClusterRequest) (*cloudv1.DeleteClusterResponse, error) {
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
	if row.State != stateDeleting {
		row.State = stateDeleting
		if err := s.deps.Store.Clusters().Update(ctx, row); err != nil {
			return nil, service.ToConnect(err)
		}
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	if err := s.deps.DeleteRecorded(ctx, records); err != nil {
		s.logger().ErrorContext(ctx, "delete cluster resources", "cluster_id", row.ID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, "could not delete the cluster's resources")
	}
	if len(records) > 0 {
		if err := s.untagSubnets(ctx, row, records[0].Namespace); err != nil {
			s.logger().ErrorContext(ctx, "untag network subnets", "cluster_id", row.ID, "error", err)
			return nil, connect.NewError(connect.CodeInternal, "could not remove the cluster's tags from the network's subnets")
		}
	}
	c, err := s.view(ctx, row)
	if err != nil {
		return nil, err
	}
	if _, err := s.finalize(ctx, row, c); err != nil {
		return nil, err
	}
	s.logger().InfoContext(ctx, "cluster deleting", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name)
	return &cloudv1.DeleteClusterResponse{Cluster: c}, nil
}
