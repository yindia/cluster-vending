package aws

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// InstanceProfileAPI is the part of the IAM client the removal of
// Karpenter's instance profiles uses.
type InstanceProfileAPI interface {
	ListInstanceProfilesForRole(context.Context, *iam.ListInstanceProfilesForRoleInput, ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error)
	ListInstanceProfileTags(context.Context, *iam.ListInstanceProfileTagsInput, ...func(*iam.Options)) (*iam.ListInstanceProfileTagsOutput, error)
	RemoveRoleFromInstanceProfile(context.Context, *iam.RemoveRoleFromInstanceProfileInput, ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error)
	DeleteInstanceProfile(context.Context, *iam.DeleteInstanceProfileInput, ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error)
}

// The tags Karpenter puts on the instance profiles it creates from an
// EC2NodeClass (karpenter.k8s.aws v1).
const (
	// TagKarpenterNodeClass names the EC2NodeClass the profile was made for.
	TagKarpenterNodeClass = "karpenter.k8s.aws/ec2nodeclass"
	// TagKarpenterManagedBy holds the name of the cluster whose Karpenter
	// manages the object.
	TagKarpenterManagedBy = "karpenter.sh/managed-by"
	// TagEKSClusterName holds the EKS cluster's name.
	TagEKSClusterName = "eks:eks-cluster-name"
	// TagClusterPrefix starts the tag kubernetes.io/cluster/<name>.
	TagClusterPrefix = "kubernetes.io/cluster/"
)

// InstanceProfiles is an IAM client that reaches the account's instance
// profiles, as the tenant role, in a region; it makes no call.
func (c *Client) InstanceProfiles(a Access, region string) (InstanceProfileAPI, error) {
	if err := c.checkAccess(a); err != nil {
		return nil, err
	}
	cfg, err := c.regionConfig(a, region)
	if err != nil {
		return nil, err
	}
	return iam.NewFromConfig(cfg), nil
}

// madeByKarpenterFor reports whether the tags are those of an instance
// profile Karpenter made for the cluster of that name: made by Karpenter
// (the EC2NodeClass tag, or the managed-by tag) and for this cluster (the
// cluster's own tag, the EKS cluster name tag, or the managed-by tag with
// its name). A profile of anything else, of another cluster's Karpenter, or
// made by hand, is never the platform's to delete.
func madeByKarpenterFor(tags map[string]string, cluster string) bool {
	if cluster == "" {
		return false
	}
	_, nodeClass := tags[TagKarpenterNodeClass]
	managedBy, hasManagedBy := tags[TagKarpenterManagedBy]
	if !nodeClass && !hasManagedBy {
		return false
	}
	if _, own := tags[TagClusterPrefix+cluster]; own {
		return true
	}
	return tags[TagEKSClusterName] == cluster || managedBy == cluster
}

// ReleaseKarpenterRole takes the role out of the instance profiles Karpenter
// made for the cluster and deletes those profiles: a profile holding the
// role blocks its deletion (DeleteConflict), and Karpenter, when its node
// classes were not deleted through it (a forced or timed-out deletion), no
// longer does it. Other profiles with the role are left alone. It returns the
// names of the profiles it deleted; every failure is returned (the others
// are still tried). What is gone already is not an error.
func ReleaseKarpenterRole(ctx context.Context, api InstanceProfileAPI, roleName, cluster string) ([]string, error) {
	var (
		deleted []string
		errs    []error
		marker  *string
	)
	for {
		out, err := api.ListInstanceProfilesForRole(ctx, &iam.ListInstanceProfilesForRoleInput{RoleName: &roleName, Marker: marker})
		if err != nil {
			if isNoSuchEntity(err) {
				return deleted, nil // the role is gone: nothing holds it
			}
			return deleted, fmt.Errorf("list the instance profiles of role %s: %w", roleName, err)
		}
		for _, p := range out.InstanceProfiles {
			if p.InstanceProfileName == nil {
				continue
			}
			name := *p.InstanceProfileName
			if ok, err := karpenterProfile(ctx, api, name, cluster); err != nil {
				errs = append(errs, err)
				continue
			} else if !ok {
				continue
			}
			if err := removeProfile(ctx, api, name, roleName); err != nil {
				errs = append(errs, err)
				continue
			}
			deleted = append(deleted, name)
		}
		if !out.IsTruncated || out.Marker == nil {
			return deleted, errors.Join(errs...)
		}
		marker = out.Marker
	}
}

// karpenterProfile reads a profile's tags and says whether Karpenter made it
// for the cluster.
func karpenterProfile(ctx context.Context, api InstanceProfileAPI, name, cluster string) (bool, error) {
	tags := map[string]string{}
	var marker *string
	for {
		out, err := api.ListInstanceProfileTags(ctx, &iam.ListInstanceProfileTagsInput{InstanceProfileName: &name, Marker: marker})
		if err != nil {
			if isNoSuchEntity(err) {
				return false, nil
			}
			return false, fmt.Errorf("read the tags of instance profile %s: %w", name, err)
		}
		for _, t := range out.Tags {
			if t.Key != nil && t.Value != nil {
				tags[*t.Key] = *t.Value
			}
		}
		if !out.IsTruncated || out.Marker == nil {
			return madeByKarpenterFor(tags, cluster), nil
		}
		marker = out.Marker
	}
}

func removeProfile(ctx context.Context, api InstanceProfileAPI, name, roleName string) error {
	if _, err := api.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: &name, RoleName: &roleName}); err != nil && !isNoSuchEntity(err) {
		return fmt.Errorf("remove role %s from instance profile %s: %w", roleName, name, err)
	}
	if _, err := api.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{InstanceProfileName: &name}); err != nil && !isNoSuchEntity(err) {
		return fmt.Errorf("delete instance profile %s: %w", name, err)
	}
	return nil
}

func isNoSuchEntity(err error) bool {
	var missing *types.NoSuchEntityException
	return errors.As(err, &missing)
}
