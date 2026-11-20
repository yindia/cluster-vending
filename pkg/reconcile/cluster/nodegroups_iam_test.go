package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// iamFake says what of Karpenter's IAM chain exists and is Ready.
type iamFake struct {
	role, access, policy bool
	attachments          int // how many of the four are Ready
	passRoleOn           string
	policyNotReady       bool
}

func passRoleDocument(resource string) string {
	raw, _ := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": []any{
		map[string]any{"Effect": "Allow", "Action": "iam:PassRole", "Resource": resource},
		map[string]any{"Effect": "Allow", "Action": []string{"ec2:RunInstances"}, "Resource": "*"}}})
	return string(raw)
}

// seed creates the fake managed objects of Karpenter's IAM chain.
func (f iamFake) seed(t *testing.T, w world, row *model.Cluster, region string) {
	t.Helper()
	prefix := crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name)
	conds := func(ready bool) xpv2.ConditionedStatus {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return xpv2.ConditionedStatus{Conditions: []xpv2.Condition{
			{Type: "Ready", Status: status, Reason: "Available"}, {Type: "Synced", Status: corev1.ConditionTrue, Reason: "ReconcileSuccess"}}}
	}
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: worldNS, Name: name} }
	create := func(o client.Object) {
		t.Helper()
		if err := w.kube.Create(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
	if f.role {
		r := &awsiam.Role{ObjectMeta: meta(prefix + "-karpenter-node")}
		r.Status.ConditionedStatus = conds(true)
		create(r)
	}
	for i, suffix := range []string{"worker", "cni", "ecr", "ssm"} {
		if i >= f.attachments {
			break
		}
		a := &awsiam.RolePolicyAttachment{ObjectMeta: meta(prefix + "-karpenter-node-" + suffix)}
		a.Status.ConditionedStatus = conds(true)
		create(a)
	}
	if f.access {
		a := &awseks.AccessEntry{ObjectMeta: meta(prefix + "-karpenter-access")}
		a.Status.ConditionedStatus = conds(true)
		create(a)
	}
	if f.policy {
		p := &awsiam.Policy{ObjectMeta: meta(crossplaneaws.ComponentPolicyName(crossplaneaws.ComponentIAMPlan{ClusterPrefix: prefix, Component: "karpenter"}))}
		p.Spec.ForProvider.Policy = ptr.To(passRoleDocument(f.passRoleOn))
		p.Status.AtProvider.Policy = ptr.To(passRoleDocument(f.passRoleOn))
		p.Status.ConditionedStatus = conds(!f.policyNotReady)
		create(p)
	}
	_ = region
}

func nodeRoleARN(row *model.Cluster) string {
	return "arn:aws:iam::444455556666:role/" + crossplaneaws.KarpenterNodeRoleName(crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name))
}

func oldNodeRoleARN(row *model.Cluster) string {
	return "arn:aws:iam::444455556666:role/" + crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name) + "-node-role"
}

// The live cluster: the platform EC2NodeClass names <prefix>-karpenter-node
// while Karpenter's policy still passes only <prefix>-node-role. Nothing
// is applied until the whole chain is Ready and the policy grants it.
func TestNodeGroupsWaitForKarpenterIAM(t *testing.T) {
	good := func(row *model.Cluster) iamFake {
		return iamFake{role: true, access: true, policy: true, attachments: 4, passRoleOn: nodeRoleARN(row)}
	}
	for _, tt := range []struct {
		name    string
		mutate  func(*iamFake, *model.Cluster)
		waitsOn string // "" applies
	}{
		{"all ready and granted", func(*iamFake, *model.Cluster) {}, ""},
		{"nothing exists yet", func(f *iamFake, _ *model.Cluster) { *f = iamFake{} }, "Role"},
		{"role missing", func(f *iamFake, _ *model.Cluster) { f.role = false }, "Role"},
		{"one attachment missing", func(f *iamFake, _ *model.Cluster) { f.attachments = 3 }, "RolePolicyAttachment"},
		{"access entry missing", func(f *iamFake, _ *model.Cluster) { f.access = false }, "AccessEntry"},
		{"policy missing", func(f *iamFake, _ *model.Cluster) { f.policy = false }, "Policy"},
		{"policy not ready", func(f *iamFake, _ *model.Cluster) { f.policyNotReady = true }, "Policy"},
		{"policy still passes the old role", func(f *iamFake, row *model.Cluster) { f.passRoleOn = oldNodeRoleARN(row) }, "iam:PassRole"},
		{"policy passes any role", func(f *iamFake, _ *model.Cluster) { f.passRoleOn = "*" }, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fw := newFakeWorkload(interceptor.Funcs{})
			w, row, _ := nodeGroupWorldSrc(t, fw)
			f := good(row)
			tt.mutate(&f, row)
			f.seed(t, w, row, "us-east-1")

			res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

			applied := fw.get(t, "EC2NodeClass", "", "platform") != nil || fw.get(t, "NodePool", "", "platform") != nil
			p := statusOf(res, "platform")
			if tt.waitsOn == "" {
				if !applied {
					t.Fatalf("the node groups were not applied although the IAM chain is ready: %v", p)
				}
				return
			}
			if applied {
				t.Fatal("the EC2NodeClass was applied before Karpenter's IAM was ready")
			}
			if p.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING ||
				!strings.Contains(p.GetMessage(), "waiting for Karpenter's IAM") || !strings.Contains(p.GetMessage(), tt.waitsOn) {
				t.Errorf("platform status = %v %q, want PENDING waiting for Karpenter's IAM (%s)", p.GetPhase(), p.GetMessage(), tt.waitsOn)
			}
		})
	}
}
