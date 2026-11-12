package component

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/fault"
)

const testIssuer = "https://oidc.eks.us-east-1.amazonaws.com/id/ABC"

func testCluster(irsa bool) *cloudv1alpha1.Cluster {
	return &cloudv1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Spec:       cloudv1alpha1.ClusterSpec{Region: "us-east-1", AWS: &cloudv1alpha1.AWSClusterSpec{IRSA: ptr.To(irsa)}},
		Status: cloudv1alpha1.ClusterStatus{OIDCIssuerURL: testIssuer,
			Endpoint: &cloudv1alpha1.ClusterEndpoint{URL: "https://ABC.gr7.us-east-1.eks.amazonaws.com"}},
	}
}

func testNetwork() *cloudv1alpha1.Network {
	return &cloudv1alpha1.Network{Status: cloudv1alpha1.NetworkStatus{
		Resources: &cloudv1alpha1.NetworkResources{CloudNetworkID: "vpc-0abc"}}}
}

func testAccount() *cloudv1alpha1.CloudAccount {
	return &cloudv1alpha1.CloudAccount{Spec: cloudv1alpha1.CloudAccountSpec{AWS: &cloudv1alpha1.AWSAccount{AccountID: "444455556666"}}}
}

func TestVarsFor(t *testing.T) {
	iam := IAMOutputs{RoleARN: "arn:aws:iam::444455556666:role/r", RoleName: "r",
		PolicyARN: "arn:aws:iam::444455556666:policy/p", NodeRoleName: "prod-1a2b-node-role"}

	v := VarsFor(testCluster(true), testNetwork(), testAccount(), iam)

	want := map[string]string{
		VarRegion: "us-east-1", VarAccountID: "444455556666", VarClusterName: "prod",
		VarClusterEndpoint: "https://ABC.gr7.us-east-1.eks.amazonaws.com", VarClusterOIDCIssuer: testIssuer,
		VarClusterOIDCProviderARN: "arn:aws:iam::444455556666:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/ABC",
		VarClusterNodeRoleName:    "prod-1a2b-node-role",
		VarClusterNodeRoleARN:     "arn:aws:iam::444455556666:role/prod-1a2b-node-role",
		VarNetworkVPCID:           "vpc-0abc",
		VarIAMRoleARN:             iam.RoleARN, VarIAMRoleName: "r", VarIAMPolicyARN: iam.PolicyARN,
		VarSystemNodeSelector: "{}", VarSystemTolerations: "[]",
	}
	for _, name := range PlaceholderNames() {
		if v[name] != want[name] {
			t.Errorf("%s = %q, want %q", name, v[name], want[name])
		}
	}
}

func TestVarsForWhatIsNotKnownYet(t *testing.T) {
	tests := []struct {
		name    string
		cluster *cloudv1alpha1.Cluster
		network *cloudv1alpha1.Network
		account *cloudv1alpha1.CloudAccount
		iam     IAMOutputs
		empty   []string
	}{
		{name: "nothing created yet", cluster: &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "prod"},
			Spec: cloudv1alpha1.ClusterSpec{Region: "us-east-1", AWS: &cloudv1alpha1.AWSClusterSpec{IRSA: ptr.To(true)}}},
			network: &cloudv1alpha1.Network{}, account: testAccount(),
			empty: []string{VarClusterEndpoint, VarClusterOIDCIssuer, VarClusterOIDCProviderARN, VarClusterNodeRoleName,
				VarClusterNodeRoleARN, VarNetworkVPCID, VarIAMRoleARN, VarIAMRoleName, VarIAMPolicyARN}},
		{name: "no account: no ARNs", cluster: testCluster(true), network: testNetwork(),
			iam:   IAMOutputs{NodeRoleName: "n"},
			empty: []string{VarAccountID, VarClusterOIDCProviderARN, VarClusterNodeRoleARN}},
		{name: "IRSA off", cluster: testCluster(false), network: testNetwork(), account: testAccount(),
			empty: []string{VarClusterOIDCProviderARN}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := VarsFor(tt.cluster, tt.network, tt.account, tt.iam)
			for _, name := range tt.empty {
				if v[name] != "" {
					t.Errorf("%s = %q, want not known", name, v[name])
				}
			}
		})
	}
}

func TestExpand(t *testing.T) {
	vars := Vars{VarClusterName: "prod", VarAccountID: "444455556666", VarIAMRoleARN: "arn:aws:iam::444455556666:role/r"}
	doc := map[string]any{
		"settings": map[string]any{"clusterName": "${cluster.name}", "replicas": float64(2)},
		"serviceAccount": map[string]any{"annotations": map[string]any{
			"eks.amazonaws.com/role-arn": "${iam.roleArn}"}},
		"args":    []any{"--policy=arn:aws:iam::${account.id}:policy/x", "--literal=$${cluster.name}"},
		"enabled": true,
	}
	orig := deepCopyAny(doc)

	got, err := Expand(doc, vars)

	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := map[string]any{
		"settings": map[string]any{"clusterName": "prod", "replicas": float64(2)},
		"serviceAccount": map[string]any{"annotations": map[string]any{
			"eks.amazonaws.com/role-arn": "arn:aws:iam::444455556666:role/r"}},
		"args":    []any{"--policy=arn:aws:iam::444455556666:policy/x", "--literal=${cluster.name}"},
		"enabled": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(doc, orig) {
		t.Error("Expand changed its input")
	}
}

func TestExpandErrors(t *testing.T) {
	tests := []struct {
		name     string
		doc      any
		vars     Vars
		notReady bool
		contains []string
	}{
		{name: "unknown name", doc: map[string]any{"a": []any{"x", "${cluster.queue}"}},
			contains: []string{"a[1]", "${cluster.queue}"}},
		{name: "unterminated", doc: map[string]any{"a": "${cluster.name"}, contains: []string{"a", "unterminated"}},
		{name: "known but empty", doc: map[string]any{"e": "${cluster.endpoint}", "r": "${iam.roleArn}"},
			vars: Vars{VarIAMRoleARN: ""}, notReady: true, contains: []string{"${cluster.endpoint}", "${iam.roleArn}"}},
		{name: "IRSA off says why", doc: "${cluster.oidcProviderArn}",
			vars:     VarsFor(testCluster(false), testNetwork(), testAccount(), IAMOutputs{}),
			notReady: true, contains: []string{"IRSA is off on cluster prod"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Expand(tt.doc, tt.vars)
			if err == nil {
				t.Fatal("want an error")
			}
			if errors.Is(err, ErrNotReady) != tt.notReady {
				t.Errorf("ErrNotReady = %v, want %v (%v)", errors.Is(err, ErrNotReady), tt.notReady, err)
			}
			if !tt.notReady && !fault.Is(err, fault.Invalid) {
				t.Errorf("want an Invalid fault, got %v", err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("%q does not name %q", err, s)
				}
			}
		})
	}
}

func TestPlaceholdersAndValidate(t *testing.T) {
	doc := map[string]any{"a": "${region}-${cluster.name}", "b": []any{"${region}", "$${x}", "${nope}"}, "c": 3}

	if got, want := Placeholders(doc), []string{VarClusterName, "nope", VarRegion}; !reflect.DeepEqual(got, want) {
		t.Errorf("Placeholders = %v, want %v", got, want)
	}
	if err := Validate(doc); err == nil || !strings.Contains(err.Error(), "b[2]") {
		t.Errorf("Validate = %v, want b[2] named", err)
	}
	delete(doc, "b")
	if err := Validate(doc); err != nil {
		t.Errorf("Validate of known names = %v", err)
	}
	if err := Validate(map[string]any{"x": "${"}); err == nil {
		t.Error("an unterminated placeholder is accepted")
	}
}

func TestExpandString(t *testing.T) {
	got, err := ExpandString("arn:aws:iam::${account.id}:policy/x", Vars{VarAccountID: "444455556666"})
	if err != nil || got != "arn:aws:iam::444455556666:policy/x" {
		t.Errorf("ExpandString = %q, %v", got, err)
	}
	if _, err := ExpandString("${account.id}", Vars{}); !errors.Is(err, ErrNotReady) {
		t.Errorf("empty account = %v, want ErrNotReady", err)
	}
}

func deepCopyAny(v any) any {
	switch d := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range d {
			out[k] = deepCopyAny(x)
		}
		return out
	case []any:
		out := make([]any, len(d))
		for i, x := range d {
			out[i] = deepCopyAny(x)
		}
		return out
	}
	return v
}
