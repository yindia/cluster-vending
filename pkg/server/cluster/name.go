package cluster

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// checkName refuses a cluster name EKS already has in the target account
// and region. The name is the EKS cluster's name, unique per account and
// region; a taken one would make Crossplane adopt (and later delete) the
// existing cluster. Checked twice: the platform's own clusters (also those
// EKS has not created yet), then AWS (clusters made elsewhere).
func (s *ClusterService) checkName(ctx context.Context, tenantID, name, region string, t target) error {
	if err := s.nameTaken(ctx, tenantID, name, region, t.account); err != nil {
		return err
	}
	if s.deps.AWSClusters == nil {
		return nil
	}
	access, err := s.deps.AWSAccess(ctx, t.account)
	if err != nil {
		return err
	}
	exists, err := s.deps.AWSClusters.EKSClusterExists(ctx, access, region, name)
	switch {
	case errors.Is(err, awscloud.ErrAccessDenied):
		return connect.NewError(connect.CodeFailedPrecondition,
			"cannot check the EKS cluster name with the account's credentials: "+err.Error())
	case err != nil:
		s.logger().ErrorContext(ctx, "check eks cluster name", "name", name, "region", region, "error", err)
		return connect.NewError(connect.CodeUnavailable, "the AWS lookup of the EKS cluster name failed: "+err.Error())
	case exists:
		return connect.NewError(connect.CodeAlreadyExists, fmt.Sprintf(
			"account %s already has an EKS cluster named %q in %s (not created by the platform); choose another name",
			t.account.AccountID, name, region))
	}
	return nil
}

// nameTaken fails with ALREADY_EXISTS when a cluster of the tenant already
// has the EKS name in the account and region.
func (s *ClusterService) nameTaken(ctx context.Context, tenantID, name, region string, account *model.CloudAccount) error {
	rows, err := s.deps.Store.Clusters().ListByName(ctx, tenantID, name)
	if err != nil {
		return service.ToConnect(err)
	}
	for _, c := range rows {
		if c.Region != region {
			continue
		}
		n, err := s.deps.Store.Networks().Get(ctx, tenantID, c.NetworkID)
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			return service.ToConnect(err)
		}
		if n.CloudAccountID == account.ID {
			return connect.NewError(connect.CodeAlreadyExists, fmt.Sprintf(
				"cluster %s (%s) already uses the EKS name %q in account %s, %s; choose another name",
				c.ID, c.State, name, account.AccountID, region))
		}
	}
	return nil
}
