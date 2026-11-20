package aws_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awsevents "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchevents/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awssqs "github.com/upbound/provider-aws/v2/apis/namespaced/sqs/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

const queueARN = "arn:aws:sqs:us-east-1:111122223333:prod-1a2b3c4d-karpenter"

func karpenterPlan() crossplaneaws.ClusterPlan {
	p := clusterPlan()
	p.Karpenter, p.KarpenterQueueARN = true, queueARN
	return p
}

func objectsByName(objects []client.Object) map[string]client.Object {
	out := map[string]client.Object{}
	for _, o := range objects {
		out[o.GetName()] = o
	}
	return out
}

func TestKarpenterNames(t *testing.T) {
	if got := crossplaneaws.KarpenterNodeRoleName("prod-1a2b3c4d"); got != "prod-1a2b3c4d-karpenter-node" {
		t.Errorf("node role = %s", got)
	}
	if got := crossplaneaws.KarpenterQueueName("prod-1a2b3c4d"); got != "prod-1a2b3c4d-karpenter" {
		t.Errorf("queue = %s", got)
	}
	// A 40-character cluster name (the API's maximum): IAM role names are
	// at most 64 characters, SQS queue names 80, EventBridge rule names 64.
	prefix := crossplaneaws.ClusterPrefix("acme", "c-1", "a"+strings.Repeat("b", 39))
	if n := len(crossplaneaws.KarpenterNodeRoleName(prefix)); n > 64 {
		t.Errorf("node role name has %d characters, IAM allows 64", n)
	}
	if n := len(crossplaneaws.KarpenterQueueName(prefix)); n > 80 {
		t.Errorf("queue name has %d characters, SQS allows 80", n)
	}
	p := karpenterPlan()
	p.Prefix = prefix
	for _, o := range crossplaneaws.ClusterResources(p) {
		if rule, ok := o.(*awsevents.Rule); ok && len(rule.Name) > 64 {
			t.Errorf("rule %s has %d characters, EventBridge allows 64", rule.Name, len(rule.Name))
		}
		if role, ok := o.(*awsiam.Role); ok && len(role.Name) > 64 {
			t.Errorf("role %s has %d characters, IAM allows 64", role.Name, len(role.Name))
		}
	}
}

func TestKarpenterNodeRoleAndAccess(t *testing.T) {
	byName := objectsByName(crossplaneaws.ClusterResources(karpenterPlan()))

	role, ok := byName["prod-1a2b3c4d-karpenter-node"].(*awsiam.Role)
	if !ok {
		t.Fatal("no karpenter node role")
	}
	if !strings.Contains(*role.Spec.ForProvider.AssumeRolePolicy, "ec2.amazonaws.com") {
		t.Errorf("trust = %s, want EC2", *role.Spec.ForProvider.AssumeRolePolicy)
	}
	var policies []string
	for _, o := range byName {
		if a, ok := o.(*awsiam.RolePolicyAttachment); ok && a.Spec.ForProvider.RoleRef.Name == role.Name {
			policies = append(policies, *a.Spec.ForProvider.PolicyArn)
		}
	}
	slices.Sort(policies)
	want := []string{
		"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly", "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
		"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy", "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"}
	if !slices.Equal(policies, want) {
		t.Errorf("policies = %v, want %v", policies, want)
	}
	entry, ok := byName["prod-1a2b3c4d-karpenter-access"].(*awseks.AccessEntry)
	if !ok {
		t.Fatal("no access entry for the karpenter node role")
	}
	fp := entry.Spec.ForProvider
	if *fp.Type != "EC2_LINUX" || fp.PrincipalArnFromRoleRef == nil || fp.PrincipalArnFromRoleRef.Name != role.Name || fp.ClusterNameRef.Name != "prod-1a2b3c4d" {
		t.Errorf("access entry = %+v", fp)
	}
}

func TestKarpenterInterruptionQueue(t *testing.T) {
	byName := objectsByName(crossplaneaws.ClusterResources(karpenterPlan()))

	queue, ok := byName["prod-1a2b3c4d-karpenter"].(*awssqs.Queue)
	if !ok {
		t.Fatal("no interruption queue")
	}
	if q := queue.Spec.ForProvider; *q.Name != "prod-1a2b3c4d-karpenter" || !*q.SqsManagedSseEnabled || *q.Region != "us-east-1" {
		t.Errorf("queue = %+v", q)
	}
	policy, ok := byName["prod-1a2b3c4d-karpenter-queue-policy"].(*awssqs.QueuePolicy)
	if !ok {
		t.Fatal("no queue policy")
	}
	if policy.Spec.ForProvider.QueueURLRef.Name != queue.Name {
		t.Errorf("queue policy refers to %v", policy.Spec.ForProvider.QueueURLRef)
	}
	var doc struct {
		Statement []struct {
			Effect, Action, Resource string
			Principal                any
		}
	}
	if err := json.Unmarshal([]byte(*policy.Spec.ForProvider.Policy), &doc); err != nil {
		t.Fatal(err)
	}
	allow := doc.Statement[0]
	services, _ := json.Marshal(allow.Principal)
	if allow.Effect != "Allow" || allow.Action != "sqs:SendMessage" || allow.Resource != queueARN ||
		!strings.Contains(string(services), "events.amazonaws.com") || !strings.Contains(string(services), "sqs.amazonaws.com") {
		t.Errorf("allow statement = %+v %s", allow, services)
	}

	patterns := map[string]string{}
	for _, o := range byName {
		rule, ok := o.(*awsevents.Rule)
		if !ok {
			continue
		}
		patterns[*rule.Spec.ForProvider.EventPattern] = rule.Name
		// The provider cannot read a rule without the bus ("cannot get ID:
		// event_bus_name cannot be empty").
		if bus := rule.Spec.ForProvider.EventBusName; bus == nil || *bus != "default" {
			t.Errorf("rule %s eventBusName = %v, want default", rule.Name, bus)
		}
		if *rule.Spec.ForProvider.Region != "us-east-1" {
			t.Errorf("rule %s region", rule.Name)
		}
	}
	for _, detailType := range []string{"EC2 Spot Instance Interruption Warning", "EC2 Instance Rebalance Recommendation",
		"EC2 Instance State-change Notification", "AWS Health Event"} {
		var found string
		for pattern, name := range patterns {
			if strings.Contains(pattern, `"`+detailType+`"`) {
				found = name
			}
		}
		if found == "" {
			t.Errorf("no rule for %q (rules: %v)", detailType, patterns)
			continue
		}
		target, ok := byName[found+"-target"].(*awsevents.Target)
		if !ok {
			t.Errorf("rule %s has no target", found)
			continue
		}
		if bus := target.Spec.ForProvider.EventBusName; bus == nil || *bus != "default" {
			t.Errorf("target of %s eventBusName = %v, want default", found, bus)
		}
		if tp := target.Spec.ForProvider; tp.RuleRef.Name != found || *tp.Arn != queueARN {
			t.Errorf("target of %s = rule %v arn %v", found, tp.RuleRef, *tp.Arn)
		}
	}
	for pattern := range patterns {
		wantSource := `"source":["aws.ec2"]`
		if strings.Contains(pattern, "AWS Health Event") {
			wantSource = `"source":["aws.health"]`
		}
		if !strings.Contains(pattern, wantSource) {
			t.Errorf("pattern %s lacks %s", pattern, wantSource)
		}
	}
}

// The queue's ARN only exists once the queue does: until then the queue
// policy and the targets (which need it) wait, the rest renders.
func TestKarpenterObjectsWaitForTheQueueARN(t *testing.T) {
	p := karpenterPlan()
	p.KarpenterQueueARN = ""

	waits := p.Waits()

	for _, kind := range []string{"QueuePolicy", "Target"} {
		if waits[kind] == "" {
			t.Errorf("%s does not wait for the queue", kind)
		}
	}
	for _, o := range crossplaneaws.ClusterResources(p) {
		switch o.(type) {
		case *awssqs.QueuePolicy, *awsevents.Target:
			t.Errorf("%T %s rendered without the queue's ARN", o, o.GetName())
		}
	}
	p.AssumeLive()
	if p.KarpenterQueueARN != crossplaneaws.KnownAfterApply {
		t.Errorf("a plan assuming the cluster live has queue ARN %q", p.KarpenterQueueARN)
	}
	if n := len(p.Waits()); n != 0 {
		t.Errorf("a live plan still waits: %v", p.Waits())
	}
}

func TestKarpenterDeletionOrder(t *testing.T) {
	var pairs []string
	for _, o := range crossplaneaws.ClusterResources(karpenterPlan()) {
		if u, ok := o.(*protectionv1beta1.Usage); ok {
			pairs = append(pairs, u.Spec.Of.ResourceRef.Name+" <- "+u.Spec.By.ResourceRef.Name)
		}
	}
	for _, want := range []string{
		"prod-1a2b3c4d-karpenter-node <- prod-1a2b3c4d-karpenter-access",
		"prod-1a2b3c4d-karpenter-node <- prod-1a2b3c4d",
		"prod-1a2b3c4d-karpenter-node-worker <- prod-1a2b3c4d",
		"prod-1a2b3c4d-karpenter <- prod-1a2b3c4d-karpenter-queue-policy",
		"prod-1a2b3c4d-karpenter <- prod-1a2b3c4d-karp-spot-target",
	} {
		if !slices.Contains(pairs, want) {
			t.Errorf("no Usage %q in %v", want, pairs)
		}
	}
}

func TestWithoutKarpenterNothingKarpenterIsRendered(t *testing.T) {
	for _, o := range crossplaneaws.ClusterResources(clusterPlan()) {
		if strings.Contains(o.GetName(), "karpenter") {
			t.Errorf("%T %s rendered for a cluster without Karpenter's setup", o, o.GetName())
		}
	}
	if clusterPlan().Waits()["Target"] != "" {
		t.Error("a cluster without Karpenter waits for its queue")
	}
}

func TestClusterListsHaveTheKarpenterKinds(t *testing.T) {
	var have []string
	for _, l := range crossplaneaws.ClusterLists() {
		name := fmt.Sprintf("%T", l)
		have = append(have, strings.TrimSuffix(name[strings.LastIndex(name, ".")+1:], "List"))
	}
	for _, kind := range []string{"Queue", "QueuePolicy", "Rule", "Target"} {
		if !slices.Contains(have, kind) {
			t.Errorf("ClusterLists lacks %s: %v", kind, have)
		}
	}
}

// With the account known, the queue's ARN is too, and nothing waits for the
// queue to exist.
func TestKarpenterQueueARNFromTheAccount(t *testing.T) {
	p := clusterPlan()
	p.Karpenter, p.AccountID = true, "111122223333"

	if w := p.Waits(); len(w) != 0 {
		t.Errorf("waits for %v", w)
	}
	var target *awsevents.Target
	for _, o := range crossplaneaws.ClusterResources(p) {
		if tg, ok := o.(*awsevents.Target); ok {
			target = tg
		}
	}
	if target == nil || *target.Spec.ForProvider.Arn != "arn:aws:sqs:us-east-1:111122223333:prod-1a2b3c4d-karpenter" {
		t.Errorf("target = %+v", target)
	}
}
