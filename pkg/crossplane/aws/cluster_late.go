package aws

import (
	"context"
	"fmt"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	awssqs "github.com/upbound/provider-aws/v2/apis/namespaced/sqs/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

// named is an empty object of the plan's namespace, to read one by name.
func (p ClusterPlan) named(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: p.Namespace, Name: name}
}

// FillLive sets the plan's values that only exist once other resources
// do, from the cluster's objects as the management cluster has them: the
// ARN of the rendered KMS key, the EKS cluster's OIDC issuer and security
// group (once it is ACTIVE), and ClusterHeld while a new cluster's log
// group is not Ready yet (a new cluster waits for its log group, or EKS
// creates the group itself; an existing cluster's group is simply
// adopted). What is still missing keeps the objects that need it out of
// the render (Waits).
func (p *ClusterPlan) FillLive(ctx context.Context, seen sequence.Observed) error {
	b := clusterBuilder{p: *p}
	eks := &awseks.Cluster{ObjectMeta: p.named(b.cluster().Name)}
	owned := seen.Owned(eks)
	if owned {
		found, err := seen.Get(ctx, eks)
		if err != nil {
			return fmt.Errorf("read EKS cluster %s: %w", eks.Name, err)
		}
		if at := eks.Status.AtProvider; found && ptr.Deref(at.Status, "") == EKSActive {
			p.OIDCIssuer, p.ClusterSecurityGroupID = issuerOf(at), ""
			if at.VPCConfig != nil {
				p.ClusterSecurityGroupID = ptr.Deref(at.VPCConfig.ClusterSecurityGroupID, "")
			}
		}
	}
	if p.CreateEncryptionKey {
		key := &awskms.Key{ObjectMeta: p.named(b.name("secrets-key"))}
		found, err := seen.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("read KMS key %s: %w", key.Name, err)
		}
		if found && seen.Owned(key) {
			p.EncryptionKeyARN = ptr.Deref(key.Status.AtProvider.Arn, "")
		}
	}
	if p.Karpenter && p.AccountID == "" {
		queue := &awssqs.Queue{ObjectMeta: p.named(KarpenterQueueName(p.Prefix))}
		found, err := seen.Get(ctx, queue)
		if err != nil {
			return fmt.Errorf("read interruption queue %s: %w", queue.Name, err)
		}
		p.KarpenterQueueARN = ""
		if found && seen.Owned(queue) {
			p.KarpenterQueueARN = ptr.Deref(queue.Status.AtProvider.Arn, "")
		}
	}
	p.ClusterHeld = false
	if p.LogGroupRetentionDays > 0 && !owned {
		ready, err := seen.Ready(ctx, &awscwl.Group{ObjectMeta: p.named(b.name("logs"))})
		if err != nil {
			return err
		}
		p.ClusterHeld = !ready
	}
	return nil
}

// issuerOf is the OIDC issuer an EKS cluster reports.
func issuerOf(at awseks.ClusterObservation) string {
	issuer := ""
	for _, id := range at.Identity {
		for _, o := range id.Oidc {
			issuer = ptr.Deref(o.Issuer, "")
		}
	}
	return issuer
}

// late reports whether the plan has objects that need a value of another
// resource (FillLive): its render then depends on what exists.
func (p ClusterPlan) late() bool {
	lt := p.NodePool.LaunchTemplate
	return p.LogGroupRetentionDays > 0 || p.CreateEncryptionKey || p.IRSA || p.Karpenter && p.AccountID == "" || (lt.Rendered() && len(lt.SecurityGroupIDs) > 0)
}

// lateRender renders a plan with what the management cluster has at the
// moment of an apply: once, for every item of the set.
type lateRender struct {
	plan    ClusterPlan
	done    bool
	err     error
	objects map[string]client.Object
	waits   map[string]string
}

// objectKey identifies a rendered object by its Go type and name.
func objectKey(obj client.Object) string { return fmt.Sprintf("%T/%s", obj, obj.GetName()) }

// build is the sequence.Item.Build of the object identity names: the
// object as the plan renders it with the live values, or what it waits
// for while the render leaves it out (Waits; a Usage waits for the
// objects it orders).
func (l *lateRender) build(identity client.Object) func(context.Context, sequence.Observed) (client.Object, string, error) {
	return func(ctx context.Context, seen sequence.Observed) (client.Object, string, error) {
		if !l.done {
			l.done = true
			if l.err = l.plan.FillLive(ctx, seen); l.err == nil {
				l.waits = l.plan.Waits()
				l.objects = map[string]client.Object{}
				for _, obj := range ClusterResources(l.plan) {
					l.objects[objectKey(obj)] = obj
				}
			}
		}
		if l.err != nil {
			return nil, "", l.err
		}
		if obj, ok := l.objects[objectKey(identity)]; ok {
			return obj, "", nil
		}
		if reason := l.waits[kindName(identity)]; reason != "" {
			return nil, reason, nil
		}
		if _, isUsage := identity.(*protectionv1beta1.Usage); isUsage {
			return nil, "the resources it orders exist", nil
		}
		return nil, "what it needs exists", nil
	}
}
