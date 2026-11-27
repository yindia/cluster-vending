package aws

import (
	"context"
	"errors"
	"slices"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
)

// sweepWorld is one account's EC2 and ELBv2 resources, by ID, with their
// tags. Filters are applied as EC2 applies them; a terminated instance
// stays listed as shutting-down for one pass, then is terminated.
type sweepWorld struct {
	instances map[string]map[string]string
	states    map[string]ec2types.InstanceStateName
	templates map[string]map[string]string
	lbs       map[string]map[string]string
	tgs       map[string]map[string]string
	sgs       map[string]map[string]string
	// sgBusy: security groups AWS refuses to delete (DependencyViolation)
	// this many times.
	sgBusy map[string]int
	// refs: security group -> the groups its rules name, ingress or
	// egress. AWS refuses to delete a group another group's rule names.
	refs map[string][]sgRef
	fail map[string]error // by call name
	ops  []string
}

func newSweepWorld() *sweepWorld {
	own := func(extra map[string]string) map[string]string {
		m := map[string]string{TagKarpenterNodePool: "default", TagClusterPrefix + "prod": "owned"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	return &sweepWorld{
		instances: map[string]map[string]string{
			"i-ours1": own(nil), "i-ours2": own(nil),
			"i-other":    {TagKarpenterNodePool: "default", TagClusterPrefix + "staging": "owned"},
			"i-nodegrp":  {TagClusterPrefix + "prod": "owned", "eks:nodegroup-name": "system"},
			"i-untagged": {},
		},
		states:    map[string]ec2types.InstanceStateName{},
		templates: map[string]map[string]string{"lt-ours": {TagKarpenterLaunchTemplateCluster: "prod"}, "lt-other": {TagKarpenterLaunchTemplateCluster: "staging"}, "lt-mine": {}},
		lbs: map[string]map[string]string{
			"arn:lb/ours": {TagLoadBalancerCluster: "prod"}, "arn:lb/other": {TagLoadBalancerCluster: "staging"}, "arn:lb/byhand": {},
		},
		tgs: map[string]map[string]string{
			"arn:tg/ours": {TagLoadBalancerCluster: "prod"}, "arn:tg/other": {TagLoadBalancerCluster: "staging"},
		},
		sgs:    map[string]map[string]string{"sg-ours": {TagLoadBalancerCluster: "prod"}, "sg-other": {TagLoadBalancerCluster: "staging"}},
		sgBusy: map[string]int{},
		refs:   map[string][]sgRef{},
		fail:   map[string]error{},
	}
}

func (w *sweepWorld) api() SweepAPI { return SweepAPI{EC2: w, ELB: w} }

func toEC2Tags(m map[string]string) []ec2types.Tag {
	var out []ec2types.Tag
	for k, v := range m {
		out = append(out, ec2types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	return out
}

// matches applies EC2's tag-key, tag:<key> and instance-state-name filters.
func matches(tags map[string]string, state ec2types.InstanceStateName, filters []ec2types.Filter) bool {
	for _, f := range filters {
		name := awssdk.ToString(f.Name)
		switch {
		case name == "tag-key":
			if _, ok := tags[f.Values[0]]; !ok {
				return false
			}
		case name == "instance-state-name":
			if !slices.Contains(f.Values, string(state)) {
				return false
			}
		case len(name) > 4 && name[:4] == "tag:":
			if v, ok := tags[name[4:]]; !ok || !slices.Contains(f.Values, v) {
				return false
			}
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (w *sweepWorld) state(id string) ec2types.InstanceStateName {
	if s, ok := w.states[id]; ok {
		return s
	}
	return ec2types.InstanceStateNameRunning
}

func (w *sweepWorld) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if err := w.fail["DescribeInstances"]; err != nil {
		return nil, err
	}
	res := ec2types.Reservation{}
	for _, id := range sortedKeys(w.instances) {
		st := w.state(id)
		if !matches(w.instances[id], st, in.Filters) {
			continue
		}
		res.Instances = append(res.Instances, ec2types.Instance{InstanceId: awssdk.String(id), Tags: toEC2Tags(w.instances[id]),
			State: &ec2types.InstanceState{Name: st}})
		if st == ec2types.InstanceStateNameShuttingDown {
			w.states[id] = ec2types.InstanceStateNameTerminated // gone by the next pass
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{res}}, nil
}

func (w *sweepWorld) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	if err := w.fail["TerminateInstances"]; err != nil {
		return nil, err
	}
	for _, id := range in.InstanceIds {
		w.ops = append(w.ops, "terminate "+id)
		w.states[id] = ec2types.InstanceStateNameShuttingDown
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (w *sweepWorld) DescribeLaunchTemplates(_ context.Context, in *ec2.DescribeLaunchTemplatesInput, _ ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error) {
	out := &ec2.DescribeLaunchTemplatesOutput{}
	for _, id := range sortedKeys(w.templates) {
		if matches(w.templates[id], "", in.Filters) {
			out.LaunchTemplates = append(out.LaunchTemplates, ec2types.LaunchTemplate{LaunchTemplateId: awssdk.String(id), Tags: toEC2Tags(w.templates[id])})
		}
	}
	return out, nil
}

func (w *sweepWorld) DeleteLaunchTemplate(_ context.Context, in *ec2.DeleteLaunchTemplateInput, _ ...func(*ec2.Options)) (*ec2.DeleteLaunchTemplateOutput, error) {
	w.ops = append(w.ops, "delete "+*in.LaunchTemplateId)
	delete(w.templates, *in.LaunchTemplateId)
	return &ec2.DeleteLaunchTemplateOutput{}, nil
}

func (w *sweepWorld) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	if out, ok := w.referencing(in.Filters); ok {
		return out, nil
	}
	out := &ec2.DescribeSecurityGroupsOutput{}
	for _, id := range sortedKeys(w.sgs) {
		if matches(w.sgs[id], "", in.Filters) {
			out.SecurityGroups = append(out.SecurityGroups, ec2types.SecurityGroup{GroupId: awssdk.String(id), Tags: toEC2Tags(w.sgs[id])})
		}
	}
	return out, nil
}

func (w *sweepWorld) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	id := *in.GroupId
	if w.referenced(id) {
		return nil, apiErr("DependencyViolation", "resource "+id+" has a dependent object")
	}
	if w.sgBusy[id] > 0 {
		w.sgBusy[id]--
		return nil, apiErr("DependencyViolation", "resource sg has a dependent object")
	}
	w.ops = append(w.ops, "delete "+id)
	delete(w.sgs, id)
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

func (w *sweepWorld) DescribeLoadBalancers(context.Context, *elbv2.DescribeLoadBalancersInput, ...func(*elbv2.Options)) (*elbv2.DescribeLoadBalancersOutput, error) {
	if err := w.fail["DescribeLoadBalancers"]; err != nil {
		return nil, err
	}
	out := &elbv2.DescribeLoadBalancersOutput{}
	for _, arn := range sortedKeys(w.lbs) {
		out.LoadBalancers = append(out.LoadBalancers, elbv2types.LoadBalancer{LoadBalancerArn: awssdk.String(arn)})
	}
	return out, nil
}

func (w *sweepWorld) DescribeTargetGroups(context.Context, *elbv2.DescribeTargetGroupsInput, ...func(*elbv2.Options)) (*elbv2.DescribeTargetGroupsOutput, error) {
	out := &elbv2.DescribeTargetGroupsOutput{}
	for _, arn := range sortedKeys(w.tgs) {
		out.TargetGroups = append(out.TargetGroups, elbv2types.TargetGroup{TargetGroupArn: awssdk.String(arn)})
	}
	return out, nil
}

func (w *sweepWorld) DescribeTags(_ context.Context, in *elbv2.DescribeTagsInput, _ ...func(*elbv2.Options)) (*elbv2.DescribeTagsOutput, error) {
	if len(in.ResourceArns) > elbTagsBatchSize {
		return nil, apiErr("ValidationError", "too many ARNs")
	}
	out := &elbv2.DescribeTagsOutput{}
	for _, arn := range in.ResourceArns {
		tags := w.lbs[arn]
		if tags == nil {
			tags = w.tgs[arn]
		}
		d := elbv2types.TagDescription{ResourceArn: awssdk.String(arn)}
		for k, v := range tags {
			d.Tags = append(d.Tags, elbv2types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
		}
		out.TagDescriptions = append(out.TagDescriptions, d)
	}
	return out, nil
}

func (w *sweepWorld) DeleteLoadBalancer(_ context.Context, in *elbv2.DeleteLoadBalancerInput, _ ...func(*elbv2.Options)) (*elbv2.DeleteLoadBalancerOutput, error) {
	w.ops = append(w.ops, "delete "+*in.LoadBalancerArn)
	delete(w.lbs, *in.LoadBalancerArn)
	return &elbv2.DeleteLoadBalancerOutput{}, nil
}

func (w *sweepWorld) DeleteTargetGroup(_ context.Context, in *elbv2.DeleteTargetGroupInput, _ ...func(*elbv2.Options)) (*elbv2.DeleteTargetGroupOutput, error) {
	w.ops = append(w.ops, "delete "+*in.TargetGroupArn)
	delete(w.tgs, *in.TargetGroupArn)
	return &elbv2.DeleteTargetGroupOutput{}, nil
}

func TestSweepNothingToSweep(t *testing.T) {
	w := &sweepWorld{instances: map[string]map[string]string{"i-other": {TagKarpenterNodePool: "x", TagClusterPrefix + "staging": "owned"}},
		states: map[string]ec2types.InstanceStateName{}, fail: map[string]error{}, sgBusy: map[string]int{}}

	r, err := SweepCluster(t.Context(), w.api(), "prod")

	if err != nil || !r.Done() || len(r.Deleted) > 0 || len(w.ops) > 0 {
		t.Fatalf("result %+v, err %v, ops %v: want nothing done", r, err, w.ops)
	}
}

// Instances are terminated, then waited for: they are left until EC2 no
// longer lists them as live. Load balancers go first, their target groups
// and security groups a pass later. Another cluster's, a managed node
// group's and untagged resources are never touched.
func TestSweepTerminatesWaitsAndOrders(t *testing.T) {
	w := newSweepWorld()

	r, err := SweepCluster(t.Context(), w.api(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Instances, []string{"i-ours1", "i-ours2"}) || !slices.Equal(r.LoadBalancers, []string{"arn:lb/ours"}) {
		t.Fatalf("first pass left %+v: the instances terminate, the load balancer is being deleted", r)
	}
	if slices.Contains(w.ops, "delete sg-ours") || slices.Contains(w.ops, "delete arn:tg/ours") {
		t.Fatalf("ops %v: the target groups and security groups wait for the load balancers", w.ops)
	}

	r, err = SweepCluster(t.Context(), w.api(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Instances, []string{"i-ours1", "i-ours2"}) {
		t.Fatalf("second pass instances %v: still shutting down", r.Instances)
	}
	r, err = SweepCluster(t.Context(), w.api(), "prod")
	if err != nil || !r.Done() {
		t.Fatalf("third pass %+v, %v: want all gone", r, err)
	}
	want := []string{"terminate i-ours1", "terminate i-ours2", "delete lt-ours", "delete arn:lb/ours", "delete arn:tg/ours", "delete sg-ours"}
	if !slices.Equal(w.ops, want) {
		t.Errorf("ops %v, want %v (each once)", w.ops, want)
	}
	for _, id := range []string{"i-other", "i-nodegrp", "i-untagged"} {
		if w.state(id) != ec2types.InstanceStateNameRunning {
			t.Errorf("%s was touched", id)
		}
	}
	if w.lbs["arn:lb/other"] == nil || w.lbs["arn:lb/byhand"] == nil || w.tgs["arn:tg/other"] == nil || w.sgs["sg-other"] == nil ||
		w.templates["lt-other"] == nil || w.templates["lt-mine"] == nil {
		t.Error("a foreign or untagged resource was deleted")
	}
}

// A security group still held by the load balancer's interfaces is left
// for the next pass, without an error.
func TestSweepSecurityGroupInUseIsLeftForLater(t *testing.T) {
	w := newSweepWorld()
	w.lbs = map[string]map[string]string{}
	w.sgBusy["sg-ours"] = 1

	r, err := SweepCluster(t.Context(), w.api(), "prod")
	if err != nil || !slices.Equal(r.SecurityGroups, []string{"sg-ours"}) {
		t.Fatalf("%+v, %v: want sg-ours left, no error", r, err)
	}
	if _, err := SweepCluster(t.Context(), w.api(), "prod"); err != nil || w.sgs["sg-ours"] != nil {
		t.Fatalf("second pass: %v, sg-ours still there: %v", err, w.sgs["sg-ours"] != nil)
	}
}

// A failing call is returned, the other steps still run, and a later pass
// that succeeds finishes the work.
func TestSweepErrorsAreReturnedAndRetried(t *testing.T) {
	w := newSweepWorld()
	w.fail["DescribeInstances"] = apiErr("Throttling", "Rate exceeded")
	w.fail["DescribeLoadBalancers"] = apiErr("Throttling", "Rate exceeded")

	_, err := SweepCluster(t.Context(), w.api(), "prod")
	if err == nil || !isCode(err, "Throttling") {
		t.Fatalf("err %v, want the throttling", err)
	}
	if w.templates["lt-ours"] != nil {
		t.Error("the launch templates are still swept when listing instances fails")
	}
	if w.sgs["sg-ours"] == nil {
		t.Error("security groups must wait while the load balancers cannot be listed")
	}
	w.fail = map[string]error{}
	for range 3 {
		if _, err = SweepCluster(t.Context(), w.api(), "prod"); err != nil {
			t.Fatal(err)
		}
	}
	if r, _ := SweepCluster(t.Context(), w.api(), "prod"); !r.Done() {
		t.Fatalf("left %v after the error cleared", r.Left())
	}
}

func TestSweepNeedsACluster(t *testing.T) {
	if _, err := SweepCluster(t.Context(), newSweepWorld().api(), ""); err == nil {
		t.Fatal("want an error without a cluster name")
	}
	if !isCode(errors.Join(errors.New("x"), apiErr("ResourceInUse", "")), "ResourceInUse") {
		t.Fatal("isCode must see through joined errors")
	}
}
