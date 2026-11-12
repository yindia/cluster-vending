package cluster

import (
	"context"
	"fmt"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// fillLive sets the plan's values that only exist once other resources do,
// from the cluster's managed resources: the ARN of the rendered KMS key,
// the EKS cluster's OIDC issuer and security group (once it is ACTIVE),
// and ClusterHeld while a new cluster's log group is not READY yet. What
// is still missing keeps the objects that need it out of the render
// (ClusterPlan.Pending).
func (s *ClusterService) fillLive(ctx context.Context, row *model.Cluster, plan *crossplaneaws.ClusterPlan) error {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return fmt.Errorf("list cluster resources: %w", err)
	}
	clusterRecorded, logGroupReady := false, false
	for _, r := range records {
		key := client.ObjectKey{Namespace: r.Namespace, Name: r.Name}
		switch r.Kind {
		case "Cluster":
			clusterRecorded = true
			eks := &awseks.Cluster{}
			if err := s.getIgnoreMissing(ctx, key, eks); err != nil {
				return fmt.Errorf("read EKS cluster %s: %w", r.Name, err)
			}
			if ptr.Deref(eks.Status.AtProvider.Status, "") != eksActiveStatus {
				continue
			}
			st, aws := &cloudv1.ClusterStatus{}, &cloudv1.AwsClusterStatus{}
			fromEKSCluster(eks.Status.AtProvider, st, aws)
			plan.OIDCIssuer, plan.ClusterSecurityGroupID = st.GetOidcIssuerUrl(), aws.GetClusterSecurityGroupId()
		case "Key":
			if !plan.CreateEncryptionKey {
				continue
			}
			k := &awskms.Key{}
			if err := s.getIgnoreMissing(ctx, key, k); err != nil {
				return fmt.Errorf("read KMS key %s: %w", r.Name, err)
			}
			plan.EncryptionKeyARN = ptr.Deref(k.Status.AtProvider.Arn, "")
		case "Group":
			views, err := s.deps.Observe(ctx, []model.ManagedResource{r})
			if err != nil {
				return err
			}
			logGroupReady = len(views) == 1 && views[0].GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
		}
	}
	// A new cluster waits for its log group, or EKS creates the group
	// itself. An existing cluster's group is simply adopted.
	plan.ClusterHeld = plan.LogGroupRetentionDays > 0 && !clusterRecorded && !logGroupReady
	return nil
}
