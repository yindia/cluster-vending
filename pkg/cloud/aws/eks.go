package aws

import (
	"context"
	"errors"
	"fmt"
	"slices"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// EKSAPI is the part of the EKS client the platform uses.
type EKSAPI interface {
	DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
	DescribeClusterVersions(context.Context, *eks.DescribeClusterVersionsInput, ...func(*eks.Options)) (*eks.DescribeClusterVersionsOutput, error)
	DescribeAddonVersions(context.Context, *eks.DescribeAddonVersionsInput, ...func(*eks.Options)) (*eks.DescribeAddonVersionsOutput, error)
}

// ClusterLookup is the server's read-only EKS lookups for CreateCluster
// and UpdateCluster.
type ClusterLookup interface {
	// EKSClusterExists checks EKS cluster names: they are unique per
	// account and region, and Crossplane would adopt an existing cluster of
	// the same name instead of failing.
	EKSClusterExists(ctx context.Context, access Access, region, name string) (bool, error)
	// ResolveEKSVersions fills the versions left empty in req.
	ResolveEKSVersions(ctx context.Context, access Access, region string, req VersionRequest) (VersionResult, error)
}

// VersionRequest is the versions of a cluster, empty where EKS should
// choose.
type VersionRequest struct {
	// KubernetesVersion "1.x"; empty is the EKS default.
	KubernetesVersion string
	Addons            []AddonVersionRequest
}

// AddonVersionRequest is one add-on; an empty Version is the version EKS
// recommends for the Kubernetes version.
type AddonVersionRequest struct{ Name, Version string }

// VersionResult is a VersionRequest with every version set.
type VersionResult struct {
	KubernetesVersion string
	Addons            map[string]AddonVersion
}

// AddonVersion is the resolved version of one add-on.
type AddonVersion struct {
	Version string
	// RequiresIAM: the add-on calls AWS and needs a role for its service
	// account.
	RequiresIAM bool
}

var _ ClusterLookup = (*Client)(nil)

// EKSClusterExists reports whether the account has an EKS cluster named
// name in region, after VerifyAccess.
func (c *Client) EKSClusterExists(ctx context.Context, access Access, region, name string) (bool, error) {
	t, err := c.verified(ctx, access, region)
	if err != nil {
		return false, err
	}
	return t.EKSClusterExists(ctx, name)
}

// ResolveEKSVersions is Tenant.ResolveEKSVersions after VerifyAccess.
func (c *Client) ResolveEKSVersions(ctx context.Context, access Access, region string, req VersionRequest) (VersionResult, error) {
	t, err := c.verified(ctx, access, region)
	if err != nil {
		return VersionResult{}, err
	}
	return t.ResolveEKSVersions(ctx, req)
}

func (c *Client) verified(ctx context.Context, access Access, region string) (*Tenant, error) {
	t, err := c.TenantFor(access, region)
	if err != nil {
		return nil, err
	}
	if err := t.VerifyAccess(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

// EKSClusterExists reports whether the tenant has an EKS cluster named
// name in its region.
func (t *Tenant) EKSClusterExists(ctx context.Context, name string) (bool, error) {
	_, err := t.eks.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &name})
	var notFound *ekstypes.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return false, nil
	}
	if err != nil {
		return false, translate(err, "describe EKS cluster "+name)
	}
	return true, nil
}

// ResolveEKSVersions sets the Kubernetes version (the EKS default when
// empty) and each add-on's version for it: the one EKS marks default when
// empty, else the requested one, which must be compatible (ErrInvalid).
func (t *Tenant) ResolveEKSVersions(ctx context.Context, req VersionRequest) (VersionResult, error) {
	k8s := req.KubernetesVersion
	if k8s == "" {
		v, err := t.defaultKubernetesVersion(ctx)
		if err != nil {
			return VersionResult{}, err
		}
		k8s = v
	}
	out := VersionResult{KubernetesVersion: k8s, Addons: make(map[string]AddonVersion, len(req.Addons))}
	for _, a := range req.Addons {
		v, err := t.addonVersion(ctx, k8s, a)
		if err != nil {
			return VersionResult{}, err
		}
		out.Addons[a.Name] = v
	}
	return out, nil
}

func (t *Tenant) defaultKubernetesVersion(ctx context.Context) (string, error) {
	out, err := t.eks.DescribeClusterVersions(ctx, &eks.DescribeClusterVersionsInput{DefaultOnly: awssdk.Bool(true)})
	if err != nil {
		return "", translate(err, "describe EKS cluster versions")
	}
	for _, v := range out.ClusterVersions {
		if v.DefaultVersion && awssdk.ToString(v.ClusterVersion) != "" {
			return awssdk.ToString(v.ClusterVersion), nil
		}
	}
	return "", fmt.Errorf("aws: EKS reports no default Kubernetes version in %s", t.Region)
}

// addonVersion picks the add-on's version for Kubernetes k8s.
func (t *Tenant) addonVersion(ctx context.Context, k8s string, a AddonVersionRequest) (AddonVersion, error) {
	var compatible []ekstypes.AddonVersionInfo // newest first, as EKS lists them
	var def *ekstypes.AddonVersionInfo
	in := &eks.DescribeAddonVersionsInput{AddonName: &a.Name, KubernetesVersion: &k8s}
	for p := eks.NewDescribeAddonVersionsPaginator(t.eks, in); p.HasMorePages(); {
		page, err := p.NextPage(ctx)
		if err != nil {
			return AddonVersion{}, translate(err, "describe versions of EKS add-on "+a.Name)
		}
		for _, info := range page.Addons {
			for _, v := range info.AddonVersions {
				for _, c := range v.Compatibilities {
					if awssdk.ToString(c.ClusterVersion) != k8s {
						continue
					}
					compatible = append(compatible, v)
					if c.DefaultVersion && def == nil {
						def = &compatible[len(compatible)-1]
					}
				}
			}
		}
	}
	if len(compatible) == 0 {
		return AddonVersion{}, fmt.Errorf("%w: EKS has no add-on %q for Kubernetes %s in %s", ErrInvalid, a.Name, k8s, t.Region)
	}
	if a.Version == "" {
		chosen := compatible[0]
		if def != nil {
			chosen = *def
		}
		return AddonVersion{Version: awssdk.ToString(chosen.AddonVersion), RequiresIAM: chosen.RequiresIamPermissions}, nil
	}
	i := slices.IndexFunc(compatible, func(v ekstypes.AddonVersionInfo) bool { return awssdk.ToString(v.AddonVersion) == a.Version })
	if i < 0 {
		return AddonVersion{}, fmt.Errorf("%w: add-on %s version %s is not compatible with Kubernetes %s (newest compatible: %s)",
			ErrInvalid, a.Name, a.Version, k8s, awssdk.ToString(compatible[0].AddonVersion))
	}
	return AddonVersion{Version: a.Version, RequiresIAM: compatible[i].RequiresIamPermissions}, nil
}
