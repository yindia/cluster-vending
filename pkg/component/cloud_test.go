package component

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/fault"
)

const testPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"eks:DescribeCluster",` +
	`"Resource":"arn:aws:eks:${region}:${account.id}:cluster/${cluster.name}"}]}`

func role(mut func(*IAMRole)) CloudResources {
	r := &IAMRole{ServiceAccounts: []ServiceAccount{{Namespace: "kube-system", Name: "sa"}}, Policy: testPolicy}
	if mut != nil {
		mut(r)
	}
	return CloudResources{IAMRole: r}
}

func TestValidateCloudResources(t *testing.T) {
	tests := []struct {
		name    string
		res     CloudResources
		wantErr string
	}{
		{name: "none", res: CloudResources{}},
		{name: "policy", res: role(nil)},
		{name: "managed only", res: role(func(r *IAMRole) {
			r.Policy, r.ManagedPolicyARNs = "", []string{"arn:aws:iam::${account.id}:policy/x"}
		})},
		{name: "statement as one object", res: role(func(r *IAMRole) {
			r.Policy = `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"s3:*","Resource":"*"}}`
		})},
		{name: "nothing to attach", res: role(func(r *IAMRole) { r.Policy = "" }), wantErr: "needs a policy"},
		{name: "no service account", res: role(func(r *IAMRole) { r.ServiceAccounts = nil }), wantErr: "service account"},
		{name: "not JSON", res: role(func(r *IAMRole) { r.Policy = "{" }), wantErr: "not a JSON object"},
		{name: "no version", res: role(func(r *IAMRole) { r.Policy = `{"Statement":[]}` }), wantErr: "no Version"},
		{name: "no statement", res: role(func(r *IAMRole) { r.Policy = `{"Version":"2012-10-17"}` }), wantErr: "no Statement"},
		{name: "unknown placeholder", res: role(func(r *IAMRole) {
			r.Policy = strings.Replace(testPolicy, "${region}", "${cluster.zone}", 1)
		}), wantErr: "${cluster.zone}"},
		{name: "own role in policy", res: role(func(r *IAMRole) {
			r.Policy = strings.Replace(testPolicy, "${region}", "${iam.roleArn}", 1)
		}), wantErr: "own role or policy"},
		{name: "long but within the limit once whitespace is dropped", res: role(func(r *IAMRole) {
			r.Policy = bigPolicy(MaxPolicySize, 4)
		})},
		{name: "over the limit without whitespace", res: role(func(r *IAMRole) {
			r.Policy = bigPolicy(MaxPolicySize+1, 0)
		}), wantErr: "without whitespace"},
		{name: "own policy in managed ARNs", res: role(func(r *IAMRole) {
			r.ManagedPolicyARNs = []string{"${iam.policyArn}"}
		}), wantErr: "managedPolicyArns[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCloudResources(tt.res)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !fault.Is(err, fault.Invalid) {
				t.Errorf("err = %v, want an Invalid fault naming %q", err, tt.wantErr)
			}
		})
	}
}

func TestExpandPolicy(t *testing.T) {
	vars := Vars{VarRegion: "us-east-1", VarAccountID: "444455556666", VarClusterName: "prod"}

	got, err := ExpandPolicy(testPolicy, vars)

	if err != nil {
		t.Fatalf("ExpandPolicy: %v", err)
	}
	var doc struct{ Statement []struct{ Resource string } }
	if err := json.Unmarshal([]byte(got), &doc); err != nil || doc.Statement[0].Resource != "arn:aws:eks:us-east-1:444455556666:cluster/prod" {
		t.Errorf("policy = %s (%v)", got, err)
	}
	if _, err := ExpandPolicy(testPolicy, Vars{VarRegion: "us-east-1"}); !errors.Is(err, ErrNotReady) {
		t.Errorf("without the account: %v, want ErrNotReady", err)
	}
	if _, err := ExpandPolicy("[]", vars); !fault.Is(err, fault.Invalid) {
		t.Errorf("not an object: %v", err)
	}
	if _, err := ExpandPolicy(strings.Replace(testPolicy, "${region}", "${iam.roleArn}", 1), vars); !fault.Is(err, fault.Invalid) {
		t.Errorf("own role: %v", err)
	}
}

func TestCheckCluster(t *testing.T) {
	tests := []struct {
		name    string
		res     CloudResources
		irsa    bool
		wantErr bool
	}{
		{name: "no cloud resources", res: CloudResources{}},
		{name: "IRSA on an IRSA cluster", res: role(nil), irsa: true},
		{name: "IRSA is the default and the cluster has it off", res: role(nil), wantErr: true},
		{name: "explicit IRSA, cluster off", res: role(func(r *IAMRole) { r.Access = IRSA }), wantErr: true},
		{name: "pod identity needs no IRSA", res: role(func(r *IAMRole) { r.Access = PodIdentity })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckCluster("aws-load-balancer-controller", tt.res, Cluster{Name: "prod", IRSA: tt.irsa})
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if !fault.Is(err, fault.FailedPrecondition) || !strings.Contains(err.Error(), "spec.aws.irsa") ||
				!strings.Contains(err.Error(), "prod") {
				t.Errorf("err = %v, want FailedPrecondition naming the cluster and spec.aws.irsa", err)
			}
		})
	}
}

func TestCloudResourcesOf(t *testing.T) {
	if got := CloudResourcesOf(nil); got.IAMRole != nil {
		t.Errorf("nil = %+v", got)
	}
	got := CloudResourcesOf(&cloudv1alpha1.ComponentCloudResources{IAMRole: &cloudv1alpha1.ComponentIAMRole{
		ServiceAccounts:   []cloudv1alpha1.ComponentServiceAccount{{Namespace: "ns", Name: "sa"}},
		ManagedPolicyARNs: []string{"arn:aws:iam::aws:policy/x"},
	}})
	if got.IAMRole == nil || got.IAMRole.Access != IRSA || got.IAMRole.ServiceAccounts[0] != (ServiceAccount{"ns", "sa"}) ||
		got.IAMRole.ManagedPolicyARNs[0] != "arn:aws:iam::aws:policy/x" {
		t.Errorf("got %+v", got.IAMRole)
	}
	pod := CloudResourcesOf(&cloudv1alpha1.ComponentCloudResources{IAMRole: &cloudv1alpha1.ComponentIAMRole{
		Access: cloudv1alpha1.ComponentIAMAccessPodIdentity}})
	if pod.IAMRole.UsesIRSA() {
		t.Error("PodIdentity uses IRSA")
	}
}

// bigPolicy is a policy document of exactly size characters other than
// whitespace, with indent spaces before each statement.
func bigPolicy(size, indent int) string {
	head, tail := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::`, `"}]}`
	pad := strings.Repeat(" ", indent)
	return pad + head + strings.Repeat("a", size-len(head)-len(tail)) + tail + pad
}
