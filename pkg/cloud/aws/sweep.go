package aws

import (
	"context"
	"errors"
	"fmt"
	"slices"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/smithy-go"
)

// The tags a cluster's controllers put on what they make in the account,
// which the deletion's sweep finds it by.
const (
	// TagKarpenterNodePool is on every instance Karpenter launched (its
	// NodePool's name).
	TagKarpenterNodePool = "karpenter.sh/nodepool"
	// TagKarpenterLaunchTemplateCluster is on the launch templates Karpenter
	// made: the EKS cluster's name.
	TagKarpenterLaunchTemplateCluster = "karpenter.k8s.aws/cluster"
	// TagLoadBalancerCluster is on the load balancers, target groups and
	// security groups the AWS Load Balancer Controller made: the EKS
	// cluster's name.
	TagLoadBalancerCluster = "elbv2.k8s.aws/cluster"
)

// Bounds of one sweep pass: what one pass lists (pages of each call) and
// how many IDs one call carries.
const (
	sweepMaxPages      = 10
	sweepPageSize      = 100
	terminateBatchSize = 100
	elbTagsBatchSize   = 20 // DescribeTags takes at most 20 ARNs
)

// SweepEC2API is the part of the EC2 client the sweep uses.
type SweepEC2API interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeLaunchTemplates(context.Context, *ec2.DescribeLaunchTemplatesInput, ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error)
	DeleteLaunchTemplate(context.Context, *ec2.DeleteLaunchTemplateInput, ...func(*ec2.Options)) (*ec2.DeleteLaunchTemplateOutput, error)
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DeleteSecurityGroup(context.Context, *ec2.DeleteSecurityGroupInput, ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
}

// SweepELBAPI is the part of the ELBv2 client the sweep uses.
type SweepELBAPI interface {
	DescribeLoadBalancers(context.Context, *elbv2.DescribeLoadBalancersInput, ...func(*elbv2.Options)) (*elbv2.DescribeLoadBalancersOutput, error)
	DescribeTargetGroups(context.Context, *elbv2.DescribeTargetGroupsInput, ...func(*elbv2.Options)) (*elbv2.DescribeTargetGroupsOutput, error)
	DescribeTags(context.Context, *elbv2.DescribeTagsInput, ...func(*elbv2.Options)) (*elbv2.DescribeTagsOutput, error)
	DeleteLoadBalancer(context.Context, *elbv2.DeleteLoadBalancerInput, ...func(*elbv2.Options)) (*elbv2.DeleteLoadBalancerOutput, error)
	DeleteTargetGroup(context.Context, *elbv2.DeleteTargetGroupInput, ...func(*elbv2.Options)) (*elbv2.DeleteTargetGroupOutput, error)
}

// SweepAPI are the clients of one account and region the sweep uses.
type SweepAPI struct {
	EC2 SweepEC2API
	ELB SweepELBAPI
}

// CloudSweep is the EC2 and ELBv2 clients of the account's tenant role in
// a region; it makes no call.
func (c *Client) CloudSweep(a Access, region string) (SweepAPI, error) {
	if err := c.checkAccess(a); err != nil {
		return SweepAPI{}, err
	}
	cfg, err := c.regionConfig(a, region)
	if err != nil {
		return SweepAPI{}, err
	}
	return SweepAPI{EC2: ec2.NewFromConfig(cfg), ELB: elbv2.NewFromConfig(cfg)}, nil
}

// SweepResult is what one pass of the sweep found still there, by ID:
// the instances not terminated yet (asked to terminate, or terminating),
// and the launch templates, load balancers, target groups and security
// groups it could not delete yet. Deleted is what the pass asked AWS to
// delete or terminate.
type SweepResult struct {
	Instances, LaunchTemplates, LoadBalancers, TargetGroups, SecurityGroups []string
	Deleted                                                                 []string
}

// Done reports whether nothing of the cluster is left.
func (r SweepResult) Done() bool { return len(r.Left()) == 0 }

// Left is every ID still there, instances first.
func (r SweepResult) Left() []string {
	return slices.Concat(r.Instances, r.LaunchTemplates, r.LoadBalancers, r.TargetGroups, r.SecurityGroups)
}

// SweepCluster is one pass of the deletion's sweep of what the cluster's
// controllers made in the account, found only by the cluster's tags:
//
//   - Karpenter's instances (karpenter.sh/nodepool, and
//     kubernetes.io/cluster/<cluster>=owned) are terminated; they are left
//     until EC2 says terminated;
//   - Karpenter's launch templates (karpenter.k8s.aws/cluster=<cluster>)
//     are deleted;
//   - the AWS Load Balancer Controller's load balancers
//     (elbv2.k8s.aws/cluster=<cluster>) are deleted, then, once none is
//     left, its target groups and its security groups.
//
// Nothing without those tags is touched. It is idempotent and bounded (a
// few pages of each call); what it could not finish is in the result, for
// the next pass. Every failure is returned (the other steps still run);
// what is gone already is not one.
func SweepCluster(ctx context.Context, api SweepAPI, cluster string) (SweepResult, error) {
	if cluster == "" {
		return SweepResult{}, errors.New("sweep: no cluster name")
	}
	var (
		r    SweepResult
		errs []error
	)
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(sweepInstances(ctx, api.EC2, cluster, &r))
	add(sweepLaunchTemplates(ctx, api.EC2, cluster, &r))
	if api.ELB != nil {
		lbErr := sweepLoadBalancers(ctx, api.ELB, cluster, &r)
		add(lbErr)
		// Target groups and security groups are in use while a load balancer
		// is: they wait for the load balancers to be gone.
		if lbErr == nil && len(r.LoadBalancers) == 0 {
			add(sweepTargetGroups(ctx, api.ELB, cluster, &r))
			add(sweepSecurityGroups(ctx, api.EC2, cluster, &r))
		}
	}
	return r, errors.Join(errs...)
}

// liveStates are the instance states of an instance not terminated yet.
var liveStates = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

func sweepInstances(ctx context.Context, api SweepEC2API, cluster string, r *SweepResult) error {
	in := &ec2.DescribeInstancesInput{MaxResults: awssdk.Int32(sweepPageSize), Filters: []ec2types.Filter{
		{Name: awssdk.String("tag-key"), Values: []string{TagKarpenterNodePool}},
		{Name: awssdk.String("tag:" + TagClusterPrefix + cluster), Values: []string{"owned"}},
		{Name: awssdk.String("instance-state-name"), Values: liveStates},
	}}
	var terminate []string
	for page := 0; page < sweepMaxPages; page++ {
		out, err := api.DescribeInstances(ctx, in)
		if err != nil {
			return fmt.Errorf("list Karpenter's instances: %w", err)
		}
		for _, res := range out.Reservations {
			for _, inst := range res.Instances {
				id := awssdk.ToString(inst.InstanceId)
				if id == "" || !karpenterInstanceOf(inst.Tags, cluster) {
					continue
				}
				r.Instances = append(r.Instances, id)
				if inst.State == nil || inst.State.Name != ec2types.InstanceStateNameShuttingDown {
					terminate = append(terminate, id)
				}
			}
		}
		if out.NextToken == nil {
			break
		}
		in.NextToken = out.NextToken
	}
	for batch := range slices.Chunk(terminate, terminateBatchSize) {
		if _, err := api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: batch}); err != nil && !isCode(err, "InvalidInstanceID.NotFound") {
			return fmt.Errorf("terminate Karpenter's instances %v: %w", batch, err)
		}
		r.Deleted = append(r.Deleted, batch...)
	}
	return nil
}

// karpenterInstanceOf checks the filters' answer again: an instance is
// the cluster's Karpenter's only with both tags.
func karpenterInstanceOf(tags []ec2types.Tag, cluster string) bool {
	m := ec2Tags(tags)
	_, pool := m[TagKarpenterNodePool]
	return pool && m[TagClusterPrefix+cluster] == "owned"
}

func sweepLaunchTemplates(ctx context.Context, api SweepEC2API, cluster string, r *SweepResult) error {
	in := &ec2.DescribeLaunchTemplatesInput{MaxResults: awssdk.Int32(sweepPageSize), Filters: []ec2types.Filter{
		{Name: awssdk.String("tag:" + TagKarpenterLaunchTemplateCluster), Values: []string{cluster}},
	}}
	var errs []error
	for page := 0; page < sweepMaxPages; page++ {
		out, err := api.DescribeLaunchTemplates(ctx, in)
		if err != nil {
			return fmt.Errorf("list Karpenter's launch templates: %w", err)
		}
		for _, lt := range out.LaunchTemplates {
			id := awssdk.ToString(lt.LaunchTemplateId)
			if id == "" || ec2Tags(lt.Tags)[TagKarpenterLaunchTemplateCluster] != cluster {
				continue
			}
			_, err := api.DeleteLaunchTemplate(ctx, &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: &id})
			switch {
			case err == nil:
				r.Deleted = append(r.Deleted, id)
			case isCode(err, "InvalidLaunchTemplateId.NotFound", "InvalidLaunchTemplateId.Malformed"):
			default:
				r.LaunchTemplates = append(r.LaunchTemplates, id)
				errs = append(errs, fmt.Errorf("delete launch template %s: %w", id, err))
			}
		}
		if out.NextToken == nil {
			break
		}
		in.NextToken = out.NextToken
	}
	return errors.Join(errs...)
}

func sweepLoadBalancers(ctx context.Context, api SweepELBAPI, cluster string, r *SweepResult) error {
	var arns []string
	in := &elbv2.DescribeLoadBalancersInput{PageSize: awssdk.Int32(sweepPageSize)}
	for page := 0; page < sweepMaxPages; page++ {
		out, err := api.DescribeLoadBalancers(ctx, in)
		if err != nil {
			return fmt.Errorf("list load balancers: %w", err)
		}
		for _, lb := range out.LoadBalancers {
			if arn := awssdk.ToString(lb.LoadBalancerArn); arn != "" {
				arns = append(arns, arn)
			}
		}
		if out.NextMarker == nil {
			break
		}
		in.Marker = out.NextMarker
	}
	own, err := taggedWith(ctx, api, arns, cluster)
	if err != nil {
		return err
	}
	var errs []error
	for _, arn := range own {
		_, err := api.DeleteLoadBalancer(ctx, &elbv2.DeleteLoadBalancerInput{LoadBalancerArn: &arn})
		if err != nil && !isCode(err, "LoadBalancerNotFound") {
			errs = append(errs, fmt.Errorf("delete load balancer %s: %w", arn, err))
		} else if err == nil {
			r.Deleted = append(r.Deleted, arn)
		}
		// Deleted or not, it is listed until a pass no longer finds it.
		r.LoadBalancers = append(r.LoadBalancers, arn)
	}
	return errors.Join(errs...)
}

func sweepTargetGroups(ctx context.Context, api SweepELBAPI, cluster string, r *SweepResult) error {
	var arns []string
	in := &elbv2.DescribeTargetGroupsInput{PageSize: awssdk.Int32(sweepPageSize)}
	for page := 0; page < sweepMaxPages; page++ {
		out, err := api.DescribeTargetGroups(ctx, in)
		if err != nil {
			return fmt.Errorf("list target groups: %w", err)
		}
		for _, tg := range out.TargetGroups {
			if arn := awssdk.ToString(tg.TargetGroupArn); arn != "" {
				arns = append(arns, arn)
			}
		}
		if out.NextMarker == nil {
			break
		}
		in.Marker = out.NextMarker
	}
	own, err := taggedWith(ctx, api, arns, cluster)
	if err != nil {
		return err
	}
	var errs []error
	for _, arn := range own {
		_, err := api.DeleteTargetGroup(ctx, &elbv2.DeleteTargetGroupInput{TargetGroupArn: &arn})
		switch {
		case err == nil:
			r.Deleted = append(r.Deleted, arn)
		case isCode(err, "TargetGroupNotFound"):
		case isCode(err, "ResourceInUse"):
			r.TargetGroups = append(r.TargetGroups, arn) // a listener still uses it: next pass
		default:
			r.TargetGroups = append(r.TargetGroups, arn)
			errs = append(errs, fmt.Errorf("delete target group %s: %w", arn, err))
		}
	}
	return errors.Join(errs...)
}

// taggedWith are the ARNs (load balancers or target groups) tagged
// elbv2.k8s.aws/cluster=<cluster>.
func taggedWith(ctx context.Context, api SweepELBAPI, arns []string, cluster string) ([]string, error) {
	var own []string
	for batch := range slices.Chunk(arns, elbTagsBatchSize) {
		out, err := api.DescribeTags(ctx, &elbv2.DescribeTagsInput{ResourceArns: batch})
		if err != nil {
			return own, fmt.Errorf("read the tags of load balancers or target groups: %w", err)
		}
		for _, d := range out.TagDescriptions {
			if elbTag(d.Tags, TagLoadBalancerCluster) == cluster {
				own = append(own, awssdk.ToString(d.ResourceArn))
			}
		}
	}
	return own, nil
}

func sweepSecurityGroups(ctx context.Context, api SweepEC2API, cluster string, r *SweepResult) error {
	in := &ec2.DescribeSecurityGroupsInput{MaxResults: awssdk.Int32(sweepPageSize), Filters: []ec2types.Filter{
		{Name: awssdk.String("tag:" + TagLoadBalancerCluster), Values: []string{cluster}},
	}}
	var errs []error
	for page := 0; page < sweepMaxPages; page++ {
		out, err := api.DescribeSecurityGroups(ctx, in)
		if err != nil {
			return fmt.Errorf("list the load balancers' security groups: %w", err)
		}
		for _, sg := range out.SecurityGroups {
			id := awssdk.ToString(sg.GroupId)
			if id == "" || ec2Tags(sg.Tags)[TagLoadBalancerCluster] != cluster {
				continue
			}
			_, err := api.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: &id})
			switch {
			case err == nil:
				r.Deleted = append(r.Deleted, id)
			case isCode(err, "InvalidGroup.NotFound"):
			case isCode(err, "DependencyViolation"):
				// The load balancer's network interfaces go some minutes
				// after it: next pass.
				r.SecurityGroups = append(r.SecurityGroups, id)
			default:
				r.SecurityGroups = append(r.SecurityGroups, id)
				errs = append(errs, fmt.Errorf("delete security group %s: %w", id, err))
			}
		}
		if out.NextToken == nil {
			break
		}
		in.NextToken = out.NextToken
	}
	return errors.Join(errs...)
}

func ec2Tags(tags []ec2types.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		if t.Key != nil {
			m[*t.Key] = awssdk.ToString(t.Value)
		}
	}
	return m
}

func elbTag(tags []elbv2types.Tag, key string) string {
	for _, t := range tags {
		if awssdk.ToString(t.Key) == key {
			return awssdk.ToString(t.Value)
		}
	}
	return ""
}

// isCode reports whether err is an AWS API error with one of codes.
func isCode(err error, codes ...string) bool {
	var api smithy.APIError
	return errors.As(err, &api) && slices.Contains(codes, api.ErrorCode())
}
