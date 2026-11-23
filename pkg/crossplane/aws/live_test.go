package aws_test

import (
	"slices"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

const readerARN = "arn:aws:iam::444455556666:role/cluster-vending-reader"

func livePlan(mutate func(*crossplaneaws.ClusterPlan)) crossplaneaws.ClusterPlan {
	p := clusterPlan()
	p.LiveProviderConfigName, p.LiveReaderRoleARN = crossplaneaws.LiveProviderConfigName("aws-1"), readerARN
	if mutate != nil {
		mutate(&p)
	}
	return p
}

func named[T client.Object](objects []client.Object, name string) T {
	var zero T
	for _, o := range objects {
		if t, ok := o.(T); ok && o.GetName() == name {
			return t
		}
	}
	return zero
}

// The live identity of PROP-02 7.1: the access entry of the reader role with
// the live group and no access policy, and the ClusterAuth through the
// account's live ProviderConfig writing "<prefix>-live-kubeconfig". The same
// for an imported cluster; none for a CONFIG_MAP cluster or without a reader
// role.
func TestLiveIdentityObjects(t *testing.T) {
	imported := func(p *crossplaneaws.ClusterPlan) {
		p.Adoption = crossplaneaws.Adoption{ClusterRoleName: "their-cluster-role", ExternalNames: map[string]string{"prod-1a2b3c4d": "prod"}}
	}
	tests := []struct {
		name string
		plan crossplaneaws.ClusterPlan
		want bool
	}{
		{name: "created", plan: livePlan(nil), want: true},
		{name: "imported", plan: livePlan(imported), want: true},
		{name: "CONFIG_MAP authentication", plan: livePlan(func(p *crossplaneaws.ClusterPlan) { p.AuthenticationMode = "CONFIG_MAP" })},
		{name: "no reader role", plan: livePlan(func(p *crossplaneaws.ClusterPlan) { p.LiveReaderRoleARN = "" })},
		{name: "API_AND_CONFIG_MAP keeps it", plan: livePlan(func(p *crossplaneaws.ClusterPlan) { p.AuthenticationMode = "API_AND_CONFIG_MAP" }), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := crossplaneaws.ClusterResources(tt.plan)
			prefix := tt.plan.Prefix
			entry := named[*awseks.AccessEntry](objects, crossplaneaws.LiveAccessEntryName(prefix))
			auth := named[*awseks.ClusterAuth](objects, crossplaneaws.LiveClusterAuthName(prefix))
			if !tt.want {
				if entry != nil || auth != nil {
					t.Fatalf("rendered a live identity: %v %v", entry, auth)
				}
				return
			}
			if entry == nil || auth == nil {
				t.Fatalf("entry %v, auth %v", entry, auth)
			}
			fp := entry.Spec.ForProvider
			if ptr.Deref(fp.PrincipalArn, "") != readerARN || ptr.Deref(fp.Type, "") != "STANDARD" ||
				len(fp.KubernetesGroups) != 1 || *fp.KubernetesGroups[0] != crossplaneaws.LiveReaderGroup ||
				fp.ClusterNameRef == nil || fp.ClusterNameRef.Name != prefix {
				t.Fatalf("access entry = %+v", fp)
			}
			if entry.Spec.ProviderConfigReference.Name != "aws-1" {
				t.Fatalf("the access entry is made by the account's own ProviderConfig: %v", entry.Spec.ProviderConfigReference)
			}
			for _, o := range objects {
				if a, ok := o.(*awseks.AccessPolicyAssociation); ok && ptr.Deref(a.Spec.ForProvider.PrincipalArn, "") == readerARN {
					t.Fatalf("the live identity has an access policy: %s", a.Name)
				}
			}
			if auth.Spec.ProviderConfigReference.Name != "aws-1-live" ||
				auth.Spec.WriteConnectionSecretToReference.Name != crossplaneaws.LiveKubeconfigSecretName(prefix) ||
				auth.Spec.ForProvider.ClusterNameRef.Name != prefix || auth.Spec.ForProvider.Region != "us-east-1" {
				t.Fatalf("cluster auth = %+v", auth.Spec)
			}
			if !crossplaneaws.IsLiveIdentity(prefix, entry.Name) || !crossplaneaws.IsLiveIdentity(prefix, auth.Name) ||
				crossplaneaws.IsLiveIdentity(prefix, prefix+"-auth") {
				t.Fatal("IsLiveIdentity")
			}
			// Both wait for the EKS cluster like the other access objects.
			for _, item := range crossplaneaws.ClusterSet(tt.plan) {
				if (item.Object.GetName() == entry.Name || item.Object.GetName() == auth.Name) && len(item.After) != 1 {
					t.Fatalf("%s waits for %d conditions", item.Object.GetName(), len(item.After))
				}
			}
		})
	}
}

func TestLiveProviderConfig(t *testing.T) {
	tests := []struct {
		name, account, tenantRole, reader, want string
	}{
		{name: "default reader role", account: "444455556666", tenantRole: "arn:aws:iam::444455556666:role/cluster-vending-provisioner", want: readerARN},
		{name: "another partition", account: "444455556666", tenantRole: "arn:aws-cn:iam::444455556666:role/p", want: "arn:aws-cn:iam::444455556666:role/cluster-vending-reader"},
		{name: "named on the account", account: "444455556666", reader: "arn:aws:iam::444455556666:role/custom/reader", want: "arn:aws:iam::444455556666:role/custom/reader"},
		{name: "no account", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := crossplaneaws.ReaderRoleARN(tt.account, tt.tenantRole, tt.reader); got != tt.want {
				t.Fatalf("ReaderRoleARN = %q, want %q", got, tt.want)
			}
		})
	}
	pc, ok := crossplaneaws.LiveProviderConfig("tenant-acme", "aws-1", "acme", readerARN, "ext-123", nil).(*awsv1beta1.ProviderConfig)
	if !ok || pc.Name != "aws-1-live" || len(pc.Spec.AssumeRoleChain) != 1 ||
		*pc.Spec.AssumeRoleChain[0].RoleARN != readerARN || *pc.Spec.AssumeRoleChain[0].ExternalID != "ext-123" {
		t.Fatalf("live ProviderConfig = %+v", pc)
	}
}

// allows reports whether rules let verb on group/resource, as RBAC
// evaluates them (no resource names, no non-resource URLs here).
func allows(rules []rbacv1.PolicyRule, verb, group, resource string) bool {
	match := func(list []string, v string) bool { return slices.Contains(list, v) || slices.Contains(list, "*") }
	for _, r := range rules {
		if match(r.Verbs, verb) && match(r.APIGroups, group) && match(r.Resources, resource) {
			return true
		}
	}
	return false
}

// The live identity reads what the projection needs and nothing else: no
// Secrets, ConfigMaps, VPAs or pod metrics, no exec or logs, no write.
func TestLiveReaderRBAC(t *testing.T) {
	rules := crossplaneaws.LiveReaderRules()
	tests := []struct {
		verb, group, resource string
		want                  bool
	}{
		{"list", "", "nodes", true},
		{"list", "", "pods", true},
		{"list", "", "events", true},
		{"list", "karpenter.sh", "nodeclaims", true},
		{"list", "wgpolicyk8s.io", "policyreports", true},
		{"list", "openreports.io", "clusterreports", true},
		{"list", "constraints.gatekeeper.sh", "k8srequiredlabels", true},
		{"list", "metrics.k8s.io", "nodes", true},
		{"get", "", "secrets", false},
		{"list", "", "secrets", false},
		{"get", "", "configmaps", false},
		{"list", "autoscaling.k8s.io", "verticalpodautoscalers", false},
		{"list", "metrics.k8s.io", "pods", false},
		{"create", "", "pods/exec", false},
		{"get", "", "pods/log", false},
		{"watch", "", "pods", false},
	}
	for _, tt := range tests {
		if got := allows(rules, tt.verb, tt.group, tt.resource); got != tt.want {
			t.Errorf("%s %s/%s = %v, want %v", tt.verb, tt.group, tt.resource, got, tt.want)
		}
	}
	for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection", "watch", "escalate", "bind", "impersonate"} {
		for _, r := range rules {
			if slices.Contains(r.Verbs, verb) || slices.Contains(r.Verbs, "*") {
				t.Errorf("rule %v has verb %s", r, verb)
			}
		}
	}
}

func TestLiveReaderObjects(t *testing.T) {
	objects, err := crossplaneaws.LiveReaderObjects()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]map[string]any{}
	for _, o := range objects {
		kinds[o.GetKind()] = o.Object
	}
	if len(kinds) != 4 {
		t.Fatalf("kinds = %v", kinds)
	}
	var binding rbacv1.ClusterRoleBinding
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(kinds["ClusterRoleBinding"], &binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef.Name != crossplaneaws.LiveReaderRole || len(binding.Subjects) != 1 ||
		binding.Subjects[0].Kind != "Group" || binding.Subjects[0].Name != crossplaneaws.LiveReaderGroup {
		t.Fatalf("binding = %+v", binding)
	}
	level := kinds["PriorityLevelConfiguration"]
	limited, _ := level["spec"].(map[string]any)["limited"].(map[string]any)
	if limited["nominalConcurrencyShares"] != int64(10) || limited["limitResponse"].(map[string]any)["type"] != "Queue" {
		t.Fatalf("priority level = %v", level)
	}
	schema := kinds["FlowSchema"]["spec"].(map[string]any)
	if schema["priorityLevelConfiguration"].(map[string]any)["name"] != crossplaneaws.LiveFlowSchema {
		t.Fatalf("flow schema = %v", schema)
	}
	subject := schema["rules"].([]any)[0].(map[string]any)["subjects"].([]any)[0].(map[string]any)
	if subject["group"].(map[string]any)["name"] != crossplaneaws.LiveReaderGroup {
		t.Fatalf("flow schema subject = %v", subject)
	}
}
