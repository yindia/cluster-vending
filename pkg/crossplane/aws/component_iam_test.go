package aws_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

const testOIDCARN = "arn:aws:iam::444455556666:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/ABC"

func componentPlan(access string) crossplaneaws.ComponentIAMPlan {
	c := clusterPlanForNames()
	return crossplaneaws.ComponentIAMPlan{
		Namespace: c.Namespace, ProviderConfigName: c.ProviderConfigName, TenantID: c.TenantID, ClusterID: c.ClusterID,
		ClusterName: c.ClusterName, ClusterPrefix: c.Prefix, Region: c.Region, Tags: map[string]string{"team": "x"},
		Component: "aws-load-balancer-controller", Access: access,
		ServiceAccounts:   []crossplaneaws.ServiceAccountPlan{{Namespace: "kube-system", Name: "aws-load-balancer-controller"}},
		Policy:            `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:Describe*","Resource":"*"}]}`,
		ManagedPolicyARNs: []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"},
	}
}

func byKind(objects []client.Object) map[string][]client.Object {
	out := map[string][]client.Object{}
	for _, obj := range objects {
		k := fmt.Sprintf("%T", obj)
		k = k[strings.LastIndex(k, ".")+1:]
		out[k] = append(out[k], obj)
	}
	return out
}

func TestComponentIAMPodIdentity(t *testing.T) {
	p := componentPlan(crossplaneaws.ComponentAccessPodIdentity)
	p.ServiceAccounts = append(p.ServiceAccounts, crossplaneaws.ServiceAccountPlan{Namespace: "kube-system", Name: "second"})

	objects := crossplaneaws.ComponentIAMResources(p)

	kinds := byKind(objects)
	want := map[string]int{"Policy": 1, "Role": 1, "RolePolicyAttachment": 2, "PodIdentityAssociation": 2,
		// 2 associations x 2 attachments, 2 attachments keep the role, 1 keeps the policy.
		"Usage": 7}
	for kind, n := range want {
		if len(kinds[kind]) != n {
			t.Errorf("%s: %d objects, want %d", kind, len(kinds[kind]), n)
		}
	}
	role := kinds["Role"][0].(*awsiam.Role)
	if got := ptr.Deref(role.Spec.ForProvider.AssumeRolePolicy, ""); !strings.Contains(got, "pods.eks.amazonaws.com") {
		t.Errorf("trust = %s, want pod identity", got)
	}
	assoc := kinds["PodIdentityAssociation"][1].(*awseks.PodIdentityAssociation)
	if ptr.Deref(assoc.Spec.ForProvider.ServiceAccount, "") != "second" || assoc.Spec.ForProvider.RoleArnRef.Name != role.Name {
		t.Errorf("association = %+v", assoc.Spec.ForProvider)
	}
	for _, obj := range objects {
		if obj.GetLabels()[crossplane.LabelComponent] != p.Component || obj.GetLabels()[crossplane.LabelCluster] != p.ClusterID {
			t.Errorf("%s labels = %v", obj.GetName(), obj.GetLabels())
		}
	}
}

func TestComponentIAMIRSATrust(t *testing.T) {
	p := componentPlan(crossplaneaws.ComponentAccessIRSA)
	p.OIDCProviderARN = testOIDCARN

	kinds := byKind(crossplaneaws.ComponentIAMResources(p))

	if len(kinds["PodIdentityAssociation"]) != 0 {
		t.Error("IRSA renders no pod identity association")
	}
	var trust struct {
		Statement []struct {
			Principal struct{ Federated string }
			Action    string
			Condition struct{ StringEquals map[string]any }
		}
	}
	role := kinds["Role"][0].(*awsiam.Role)
	if err := json.Unmarshal([]byte(ptr.Deref(role.Spec.ForProvider.AssumeRolePolicy, "")), &trust); err != nil {
		t.Fatal(err)
	}
	st := trust.Statement[0]
	issuer := "oidc.eks.us-east-1.amazonaws.com/id/ABC"
	if st.Principal.Federated != testOIDCARN || st.Action != "sts:AssumeRoleWithWebIdentity" ||
		st.Condition.StringEquals[issuer+":aud"] != "sts.amazonaws.com" {
		t.Errorf("trust = %+v", st)
	}
	subs, _ := st.Condition.StringEquals[issuer+":sub"].([]any)
	if len(subs) != 1 || subs[0] != "system:serviceaccount:kube-system:aws-load-balancer-controller" {
		t.Errorf("subjects = %v", st.Condition.StringEquals[issuer+":sub"])
	}
}

func TestComponentIAMIRSARoleWaitsForTheOIDCProvider(t *testing.T) {
	p := componentPlan(crossplaneaws.ComponentAccessIRSA)
	set := crossplaneaws.ComponentIAMSet(p)
	ctx := context.Background()

	var build func() (client.Object, string, error)
	for _, item := range set {
		if _, ok := item.Object.(*awsiam.Role); ok {
			if item.Build == nil {
				t.Fatal("an IRSA role without a known provider is built at apply time")
			}
			build = func() (client.Object, string, error) { return item.Build(ctx, seenOf(nil)) }
			obj, waits, err := build()
			if err != nil || obj != nil || !strings.Contains(waits, "OIDC provider") {
				t.Fatalf("without provider: %v, %q, %v", obj, waits, err)
			}
			provider := &awsiam.OpenIDConnectProvider{}
			provider.Name = p.ClusterPrefix + "-oidc"
			provider.Status.AtProvider.Arn = ptr.To(testOIDCARN)
			obj, waits, err = item.Build(ctx, seenOf(provider))
			role, ok := obj.(*awsiam.Role)
			if err != nil || waits != "" || !ok || !strings.Contains(ptr.Deref(role.Spec.ForProvider.AssumeRolePolicy, ""), testOIDCARN) {
				t.Fatalf("with provider: %v, %q, %v", obj, waits, err)
			}
		}
	}
	if build == nil {
		t.Fatal("the set has no role")
	}
}

func seenOf(objects ...client.Object) seen {
	s := seen{objects: map[string]client.Object{}, ready: map[string]bool{}, owned: map[string]bool{}}
	for _, obj := range objects {
		if obj != nil {
			s.objects[seenKey(obj)] = obj
		}
	}
	return s
}

func TestComponentIAMSetOrder(t *testing.T) {
	set := crossplaneaws.ComponentIAMSet(componentPlan(crossplaneaws.ComponentAccessPodIdentity))
	for _, item := range set {
		switch obj := item.Object.(type) {
		case *awsiam.Policy, *awsiam.Role:
			if len(item.After) != 0 {
				t.Errorf("%s waits for %d conditions, want none", obj.GetName(), len(item.After))
			}
		case *awsiam.RolePolicyAttachment, *awseks.PodIdentityAssociation, *protectionv1beta1.Usage:
			if len(item.After) == 0 {
				t.Errorf("%T %s waits for nothing", obj, obj.GetName())
			}
		}
	}
}

func TestComponentIAMNamesFitIAM(t *testing.T) {
	p := componentPlan(crossplaneaws.ComponentAccessPodIdentity)
	p.ClusterPrefix = crossplaneaws.ClusterPrefix("acme", "c-1", strings.Repeat("c", 40))
	p.Component = strings.Repeat("k", 63)
	p.ServiceAccounts = make([]crossplaneaws.ServiceAccountPlan, 8)
	for i := range p.ServiceAccounts {
		p.ServiceAccounts[i] = crossplaneaws.ServiceAccountPlan{Namespace: "ns", Name: "sa"}
	}
	other := p
	other.Component += "x"
	if crossplaneaws.ComponentRoleName(p) == crossplaneaws.ComponentRoleName(other) {
		t.Error("two components share a role name")
	}
	for _, obj := range crossplaneaws.ComponentIAMResources(p) {
		if errs := validation.IsDNS1123Subdomain(obj.GetName()); len(errs) > 0 {
			t.Errorf("%T %q: %v", obj, obj.GetName(), errs)
		}
		if _, isRole := obj.(*awsiam.Role); isRole && len(obj.GetName()) > 64 {
			t.Errorf("role name %q is longer than IAM allows", obj.GetName())
		}
		if _, isUsage := obj.(*protectionv1beta1.Usage); isUsage && len(obj.GetName()) > 63 {
			t.Errorf("usage name %q is too long", obj.GetName())
		}
	}
	short := componentPlan(crossplaneaws.ComponentAccessIRSA)
	if got := crossplaneaws.ComponentRoleName(short); got != short.ClusterPrefix+"-aws-load-balancer-controller-role" {
		t.Errorf("short role name = %q", got)
	}
}

func TestNodeRoleName(t *testing.T) {
	p := clusterPlan()
	if got := crossplaneaws.NodeRoleName(p); got != p.Prefix+"-node-role" {
		t.Errorf("rendered = %q", got)
	}
	p.NodeRoleName = "imported-nodes"
	if got := crossplaneaws.NodeRoleName(p); got != "imported-nodes" {
		t.Errorf("imported = %q", got)
	}
}
