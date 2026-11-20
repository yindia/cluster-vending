package cluster

import (
	"context"
	"time"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

// releaseProfilesTimeout bounds the IAM calls of one pass.
const releaseProfilesTimeout = 30 * time.Second

// releaseNodeProfiles takes Karpenter's node role out of the instance
// profiles Karpenter made for this cluster, and deletes those profiles,
// before the role is deleted (stage 5): a profile that still holds the role
// fails its deletion (DeleteConflict) for ever. Karpenter removes its own
// profiles when its EC2NodeClasses are deleted through it; after a forced or
// timed-out node group stage they never were. Only profiles carrying
// Karpenter's tags for this cluster are touched (awscloud.ReleaseKarpenterRole).
// It runs while the node role is still to be deleted, and is idempotent. A
// failure is logged and tried again next pass: it never holds the deletion
// itself (the role's deletion says what is left).
func (s *Reconciler) releaseNodeProfiles(ctx context.Context, row *model.Cluster, c *clusterRemoval) {
	if s.deps.InstanceProfiles == nil {
		return
	}
	roleName := crossplaneaws.KarpenterNodeRoleName(c.prefix)
	if !s.nodeRoleLeft(c, roleName) {
		return
	}
	log := s.logger().With("cluster_id", row.ID, "role", roleName)
	account, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, row.TenantID, row.CloudAccountID)
	if err != nil {
		log.WarnContext(ctx, "instance profiles of Karpenter's node role not checked: account unreadable", "error", err)
		return
	}
	access, err := s.deps.AWSAccess(ctx, account)
	if err != nil {
		log.WarnContext(ctx, "instance profiles of Karpenter's node role not checked: account unreachable", "error", err)
		return
	}
	api, err := s.deps.InstanceProfiles(access, row.Region)
	if err != nil {
		log.WarnContext(ctx, "instance profiles of Karpenter's node role not checked: account unreachable", "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, releaseProfilesTimeout)
	defer cancel()
	deleted, err := awscloud.ReleaseKarpenterRole(ctx, api, roleName, row.Name)
	if len(deleted) > 0 {
		log.InfoContext(ctx, "instance profiles of Karpenter's node role deleted", "profiles", deleted)
	}
	if err != nil {
		log.WarnContext(ctx, "instance profiles of Karpenter's node role not all deleted; the role's deletion waits", "error", err)
	}
}

// nodeRoleLeft reports whether Karpenter's node role is recorded and not
// gone yet.
func (s *Reconciler) nodeRoleLeft(c *clusterRemoval, roleName string) bool {
	for _, r := range c.records {
		if r.Kind == "Role" && r.Name == roleName && !c.gone(r) {
			return true
		}
	}
	return false
}
