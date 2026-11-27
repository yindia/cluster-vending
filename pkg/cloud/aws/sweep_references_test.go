package aws

import (
	"context"
	"slices"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// sgRef is one rule of a security group that names another group.
type sgRef struct {
	group  string
	egress bool
}

func refFilter(name string) (egress, ok bool) {
	switch name {
	case "ip-permission.group-id":
		return false, true
	case "egress.ip-permission.group-id":
		return true, true
	}
	return false, false
}

// referencing answers a DescribeSecurityGroups filtered by the group its
// rules name, as EC2 does: the groups with such a rule, with the rules.
func (w *sweepWorld) referencing(filters []ec2types.Filter) (*ec2.DescribeSecurityGroupsOutput, bool) {
	if len(filters) != 1 {
		return nil, false
	}
	egress, ok := refFilter(awssdk.ToString(filters[0].Name))
	if !ok {
		return nil, false
	}
	target := filters[0].Values[0]
	out := &ec2.DescribeSecurityGroupsOutput{}
	for _, id := range sortedKeys(w.refs) {
		g := ec2types.SecurityGroup{GroupId: awssdk.String(id)}
		for _, r := range w.refs[id] {
			if r.group != target || r.egress != egress {
				continue
			}
			p := ec2types.IpPermission{IpProtocol: awssdk.String("tcp"), FromPort: awssdk.Int32(80), ToPort: awssdk.Int32(80),
				UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: awssdk.String(r.group)}}}
			if egress {
				g.IpPermissionsEgress = append(g.IpPermissionsEgress, p)
			} else {
				g.IpPermissions = append(g.IpPermissions, p)
			}
		}
		if len(g.IpPermissions)+len(g.IpPermissionsEgress) > 0 {
			out.SecurityGroups = append(out.SecurityGroups, g)
		}
	}
	return out, true
}

func (w *sweepWorld) referenced(id string) bool {
	for _, refs := range w.refs {
		if slices.ContainsFunc(refs, func(r sgRef) bool { return r.group == id }) {
			return true
		}
	}
	return false
}

func (w *sweepWorld) revoke(group string, perms []ec2types.IpPermission, egress bool) {
	for _, p := range perms {
		for _, pair := range p.UserIdGroupPairs {
			target := awssdk.ToString(pair.GroupId)
			w.ops = append(w.ops, "revoke "+group+" -> "+target)
			w.refs[group] = slices.DeleteFunc(w.refs[group], func(r sgRef) bool { return r.group == target && r.egress == egress })
		}
	}
}

func (w *sweepWorld) RevokeSecurityGroupIngress(_ context.Context, in *ec2.RevokeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	if err := w.fail["RevokeSecurityGroupIngress"]; err != nil {
		return nil, err
	}
	w.revoke(awssdk.ToString(in.GroupId), in.IpPermissions, false)
	return &ec2.RevokeSecurityGroupIngressOutput{}, nil
}

func (w *sweepWorld) RevokeSecurityGroupEgress(_ context.Context, in *ec2.RevokeSecurityGroupEgressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupEgressOutput, error) {
	w.revoke(awssdk.ToString(in.GroupId), in.IpPermissions, true)
	return &ec2.RevokeSecurityGroupEgressOutput{}, nil
}

// The AWS Load Balancer Controller's backend group is named by rules in
// the EKS cluster's security group, which goes only with the cluster, a
// later stage. The sweep removes those rules (they name a group being
// deleted) and then deletes the group, instead of waiting forever.
func TestSweepRemovesRulesThatNameTheLoadBalancerGroup(t *testing.T) {
	w := newSweepWorld()
	w.lbs = map[string]map[string]string{}
	w.refs["sg-eks-cluster"] = []sgRef{{group: "sg-ours"}, {group: "sg-other"}}
	w.refs["sg-nodes"] = []sgRef{{group: "sg-ours", egress: true}}

	for pass := 0; pass < 3 && w.sgs["sg-ours"] != nil; pass++ {
		if _, err := SweepCluster(t.Context(), w.api(), "prod"); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if w.sgs["sg-ours"] != nil {
		t.Fatalf("sg-ours is still there after three passes; ops: %v", w.ops)
	}
	if got := w.refs["sg-eks-cluster"]; !slices.Equal(got, []sgRef{{group: "sg-other"}}) {
		t.Fatalf("the rule naming another cluster's group must stay: %v", got)
	}
	if len(w.refs["sg-nodes"]) != 0 {
		t.Fatalf("the egress rule naming sg-ours must go: %v", w.refs["sg-nodes"])
	}
}

// A failure to remove a rule is returned, and the group is left for a
// later pass.
func TestSweepRuleRemovalFailureIsReturned(t *testing.T) {
	w := newSweepWorld()
	w.lbs = map[string]map[string]string{}
	w.refs["sg-eks-cluster"] = []sgRef{{group: "sg-ours"}}
	w.fail["RevokeSecurityGroupIngress"] = apiErr("UnauthorizedOperation", "not allowed")

	r, err := SweepCluster(t.Context(), w.api(), "prod")
	if err == nil || !slices.Contains(r.SecurityGroups, "sg-ours") {
		t.Fatalf("%+v, %v: want an error and sg-ours left", r, err)
	}
}
