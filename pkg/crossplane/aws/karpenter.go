package aws

import (
	"encoding/json"

	awsevents "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchevents/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awssqs "github.com/upbound/provider-aws/v2/apis/namespaced/sqs/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// What a cluster with Karpenter node groups needs in AWS beyond what its
// managed node group has (docs/architecture.md#node-layout):
//
//   - the role Karpenter's nodes run as, "<prefix>-karpenter-node", with the
//     four AWS managed policies a node needs, and an explicit EC2_LINUX
//     access entry for it (node access then does not hang on the managed
//     group's own entry);
//   - the interruption queue "<prefix>-karpenter" (SQS, server-side
//     encrypted) with a policy that lets EventBridge send to it, and
//     EventBridge rules with targets for Spot interruption warnings,
//     rebalance recommendations, instance state changes and AWS Health
//     events.
//
// Karpenter creates the instance profile itself from EC2NodeClass.spec.role
// (its controller policy may PassRole this role, nothing else).

const (
	policySSMCore = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"

	accessEntryEC2Linux = "EC2_LINUX"

	// defaultEventBus is the bus the rules and targets are on. The provider
	// cannot read a rule or target without it ("cannot get ID:
	// event_bus_name cannot be empty").
	defaultEventBus = "default"

	// karpenterTargetID names the queue as the rules' target.
	karpenterTargetID = "karpenter-interruption-queue"
	// karpenterQueueRetentionSeconds is how long the queue keeps a message
	// (Karpenter's own CloudFormation uses 300).
	karpenterQueueRetentionSeconds = 300
)

// KarpenterNodeRoleName is the IAM name of the role Karpenter's nodes run
// as: "<prefix>-karpenter-node" (at most 64 characters with a 40-character
// cluster name: prefix 49 + 15).
func KarpenterNodeRoleName(prefix string) string { return prefix + "-karpenter-node" }

// KarpenterQueueName is the interruption queue's name,
// EC2NodeClass-independent and what Karpenter's settings.interruptionQueue
// takes: "<prefix>-karpenter".
func KarpenterQueueName(prefix string) string { return prefix + "-karpenter" }

// karpenterRule is one EventBridge rule feeding the queue.
type karpenterRule struct {
	// suffix names the rule "<prefix>-<suffix>" (at most 15 characters:
	// EventBridge allows 64 for the whole name).
	suffix, source, detailType string
}

var karpenterRules = []karpenterRule{
	{"karp-spot", "aws.ec2", "EC2 Spot Instance Interruption Warning"},
	{"karp-rebalance", "aws.ec2", "EC2 Instance Rebalance Recommendation"},
	{"karp-state", "aws.ec2", "EC2 Instance State-change Notification"},
	{"karp-health", "aws.health", "AWS Health Event"},
}

func (r karpenterRule) pattern() string {
	raw, _ := json.Marshal(map[string]any{"source": []string{r.source}, "detail-type": []string{r.detailType}}) // strings: cannot fail
	return string(raw)
}

// queueARN is the interruption queue's ARN: given, built from the account,
// or "" while it is not known.
func (p ClusterPlan) queueARN() string {
	switch {
	case p.KarpenterQueueARN != "":
		return p.KarpenterQueueARN
	case p.AccountID != "":
		return "arn:" + Partition(p.Region) + ":sqs:" + p.Region + ":" + p.AccountID + ":" + KarpenterQueueName(p.Prefix)
	}
	return ""
}

// karpenterObjects are the objects above, in creation order, when the plan
// has Karpenter's setup. The queue policy and the targets need the queue's
// ARN (KarpenterQueueARN) and are left out until it is known (Waits).
func (b clusterBuilder) karpenterObjects() []client.Object {
	if !b.p.Karpenter {
		return nil
	}
	role := b.roleNamed("karpenter-node", trustEC2, "")
	out := []client.Object{role}
	for _, a := range []struct{ suffix, policy string }{
		{"karpenter-node-worker", policyWorkerNode}, {"karpenter-node-cni", policyCNI},
		{"karpenter-node-ecr", policyECRRead}, {"karpenter-node-ssm", policySSMCore},
	} {
		out = append(out, b.attach(a.suffix, "karpenter-node", a.policy))
	}
	out = append(out, b.karpenterAccessEntry(), b.karpenterQueue(), b.karpenterQueuePolicy())
	for _, r := range karpenterRules {
		out = append(out, b.karpenterRule(r), b.karpenterTarget(r))
	}
	return out
}

// karpenterAccessEntry is the EC2_LINUX access entry of the node role, by
// reference to the role (its ARN needs the account, which the plan does
// not have).
func (b clusterBuilder) karpenterAccessEntry() *awseks.AccessEntry {
	entry := b.accessEntryOf("karpenter-access", "", accessEntryEC2Linux)
	entry.Spec.ForProvider.PrincipalArn = nil
	entry.Spec.ForProvider.PrincipalArnFromRoleRef = b.ref(b.name("karpenter-node"))
	return entry
}

func (b clusterBuilder) karpenterQueue() *awssqs.Queue {
	name := KarpenterQueueName(b.p.Prefix)
	return &awssqs.Queue{ObjectMeta: b.meta(name),
		Spec: awssqs.QueueSpec{ManagedResourceSpec: b.spec(), ForProvider: awssqs.QueueParameters{
			Region: ptr.To(b.p.Region), Name: ptr.To(name), SqsManagedSseEnabled: ptr.To(true),
			MessageRetentionSeconds: ptr.To(float64(karpenterQueueRetentionSeconds)), Tags: b.tags(name),
		}}}
}

func (b clusterBuilder) karpenterQueuePolicy() *awssqs.QueuePolicy {
	arn := b.p.queueARN()
	return &awssqs.QueuePolicy{ObjectMeta: b.meta(b.name("karpenter-queue-policy")),
		Spec: awssqs.QueuePolicySpec{ManagedResourceSpec: b.spec(), ForProvider: awssqs.QueuePolicyParameters{
			Region: ptr.To(b.p.Region), QueueURLRef: b.ref(KarpenterQueueName(b.p.Prefix)),
			Policy: ptr.To(queuePolicyDocument(arn)),
		}}}
}

// queuePolicyDocument lets EventBridge and SQS send to the queue, and
// refuses anything not over TLS (Karpenter's own template does both).
func queuePolicyDocument(arn string) string {
	raw, _ := json.Marshal(map[string]any{"Version": "2012-10-17", "Id": "EC2InterruptionPolicy", "Statement": []any{
		map[string]any{"Effect": "Allow", "Principal": map[string]any{"Service": []string{"events.amazonaws.com", "sqs.amazonaws.com"}},
			"Action": "sqs:SendMessage", "Resource": arn},
		map[string]any{"Sid": "DenyHTTP", "Effect": "Deny", "Principal": "*", "Action": "sqs:*", "Resource": arn,
			"Condition": map[string]any{"Bool": map[string]any{"aws:SecureTransport": "false"}}},
	}}) // plain values: cannot fail
	return string(raw)
}

func (b clusterBuilder) karpenterRule(r karpenterRule) *awsevents.Rule {
	name := b.name(r.suffix)
	return &awsevents.Rule{ObjectMeta: b.meta(name),
		Spec: awsevents.RuleSpec{ManagedResourceSpec: b.spec(), ForProvider: awsevents.RuleParameters{
			Region: ptr.To(b.p.Region), EventBusName: ptr.To(defaultEventBus),
			Description:  ptr.To("Karpenter interruption events of EKS cluster " + b.p.ClusterName),
			EventPattern: ptr.To(r.pattern()), Tags: b.tags(name),
		}}}
}

func (b clusterBuilder) karpenterTarget(r karpenterRule) *awsevents.Target {
	return &awsevents.Target{ObjectMeta: b.meta(b.name(r.suffix) + "-target"),
		Spec: awsevents.TargetSpec{ManagedResourceSpec: b.spec(), ForProvider: awsevents.TargetParameters{
			Region: ptr.To(b.p.Region), EventBusName: ptr.To(defaultEventBus), RuleRef: b.ref(b.name(r.suffix)), TargetID: ptr.To(karpenterTargetID),
			Arn: ptr.To(b.p.queueARN()),
		}}}
}

// karpenterUsagePairs are the deletion dependencies of the objects above:
// the node role (and its policies) outlive the EKS cluster, the role its
// access entry; the queue outlives its policy and the targets, the rules
// their targets.
func (b clusterBuilder) karpenterUsagePairs(eks UsageRef) []UsagePair {
	if !b.p.Karpenter {
		return nil
	}
	role := roleKind.named(b.name("karpenter-node"))
	queue := queueKind.named(KarpenterQueueName(b.p.Prefix))
	pairs := []UsagePair{
		{Of: role, By: accessEntryKind.named(b.name("karpenter-access"))},
		{Of: role, By: eks},
		{Of: queue, By: queuePolicyKind.named(b.name("karpenter-queue-policy"))},
	}
	for _, a := range []string{"karpenter-node-worker", "karpenter-node-cni", "karpenter-node-ecr", "karpenter-node-ssm"} {
		pairs = append(pairs, UsagePair{Of: attachmentKind.named(b.name(a)), By: eks})
	}
	for _, r := range karpenterRules {
		target := targetKind.named(b.name(r.suffix) + "-target")
		pairs = append(pairs, UsagePair{Of: ruleKind.named(b.name(r.suffix)), By: target}, UsagePair{Of: queue, By: target})
	}
	return pairs
}

// karpenterLists are empty lists of the kinds above, for ClusterLists.
func karpenterLists() []client.ObjectList {
	return []client.ObjectList{&awssqs.QueueList{}, &awssqs.QueuePolicyList{}, &awsevents.RuleList{}, &awsevents.TargetList{}}
}

// KarpenterNodeIAMObjects are the identities (namespace and name only) of
// what the EC2NodeClasses need before Karpenter can launch a node: the node
// role, its four policy attachments and its EC2_LINUX access entry, as
// karpenterObjects renders them.
func KarpenterNodeIAMObjects(namespace, prefix string) []client.Object {
	b := clusterBuilder{p: ClusterPlan{Namespace: namespace, Prefix: prefix}}
	out := []client.Object{b.roleNamed("karpenter-node", trustEC2, "")}
	for _, suffix := range []string{"karpenter-node-worker", "karpenter-node-cni", "karpenter-node-ecr", "karpenter-node-ssm"} {
		out = append(out, b.attach(suffix, "karpenter-node", ""))
	}
	return append(out, b.karpenterAccessEntry())
}
